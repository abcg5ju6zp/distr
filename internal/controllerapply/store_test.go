package controllerapply

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/distr-sh/distr/internal/types"
	"github.com/google/uuid"
	. "github.com/onsi/gomega"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

func target() Target {
	return Target{
		DeploymentID: uuid.New(),
		RevisionID:   uuid.New(),
		ProjectName:  "example",
		DockerType:   types.DockerTypeCompose,
	}
}

func mustClaim(g Gomega, s *Store, target Target) (*Generation, bool) {
	gen, adopted, _, err := s.Claim(target)
	g.Expect(err).NotTo(HaveOccurred())
	return gen, adopted
}

func TestClaimIsUniquePerRevisionAndAdopted(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	target := target()

	unlock, locked, err := s.TryLock(target.DeploymentID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(locked).To(BeTrue())

	first, adopted := mustClaim(g, s, target)
	g.Expect(adopted).To(BeFalse())
	g.Expect(first.ID).NotTo(Equal(uuid.Nil))

	// Another tick or a restarted process claims the same revision and must continue the same generation.
	again, adopted := mustClaim(g, s, target)
	g.Expect(adopted).To(BeTrue())
	g.Expect(again.ID).To(Equal(first.ID))
	unlock()

	// A second process that cannot take the lock is expected to skip the tick.
	_, locked, err = s.TryLock(target.DeploymentID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(locked).To(BeTrue())
}

func TestLockSerializesDeployments(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	id := uuid.New()

	unlock, locked, err := s.TryLock(id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(locked).To(BeTrue())

	_, locked, err = s.TryLock(id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(locked).To(BeFalse())

	unlock()

	_, locked, err = s.TryLock(id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(locked).To(BeTrue())
}

func TestNewerRevisionSupersedesGeneration(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	target := target()

	unlock, _, err := s.TryLock(target.DeploymentID)
	g.Expect(err).NotTo(HaveOccurred())
	defer unlock()

	old, _ := mustClaim(g, s, target)

	target.RevisionID = uuid.New()
	next, adopted := mustClaim(g, s, target)
	g.Expect(adopted).To(BeFalse())
	g.Expect(next.ID).NotTo(Equal(old.ID))

	// The old generation must not be able to publish state anymore.
	err = s.PublishState(DeploymentState{
		ID: target.DeploymentID, RevisionID: old.RevisionID, GenerationID: old.ID, State: StateReady,
	})
	g.Expect(err).To(MatchError(ErrGenerationSuperseded))

	// The current generation can publish.
	g.Expect(s.PublishState(DeploymentState{
		ID: target.DeploymentID, RevisionID: next.RevisionID, GenerationID: next.ID, State: StateProgressing,
	})).To(Succeed())
}

func TestPublishStateWithoutGenerationRejectedAfterUninstall(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	id := uuid.New()
	gen := uuid.New()

	// No generations on disk: a generation-owned publish must not resurrect an uninstalled deployment.
	err := s.PublishState(DeploymentState{ID: id, RevisionID: uuid.New(), GenerationID: gen})
	g.Expect(err).To(MatchError(ErrNoGeneration))
}

func TestPendingGenerationsRetriedFromReceipt(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	target := target()

	unlock, _, err := s.TryLock(target.DeploymentID)
	g.Expect(err).NotTo(HaveOccurred())
	gen, _ := mustClaim(g, s, target)

	gen.Phase = PhaseFinalized
	gen.Receipt = &Receipt{
		Action:        ActionComposeUp,
		Outcome:       OutcomeSucceeded,
		Reported:      false,
		ReportType:    types.DeploymentStatusTypeProgressing,
		ReportMessage: "compose command executed successfully",
	}
	g.Expect(s.SaveGeneration(gen)).To(Succeed())
	unlock()

	// The finalized generation is adopted so only the missing callback is retried, never the apply.
	adoptedGen, adopted := mustClaim(g, s, target)
	g.Expect(adopted).To(BeTrue())
	g.Expect(adoptedGen.Phase).To(Equal(PhaseFinalized))

	pending, err := s.PendingGenerations()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pending).To(HaveLen(1))
	g.Expect(pending[0].Receipt.ReportMessage).To(Equal("compose command executed successfully"))

	pending[0].Receipt.Reported = true
	g.Expect(s.SaveGeneration(pending[0])).To(Succeed())

	pending, err = s.PendingGenerations()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pending).To(BeEmpty())

	// A reported finalized generation must not be adopted anymore.
	_, adopted = mustClaim(g, s, target)
	g.Expect(adopted).To(BeFalse())
}

func TestLoadStatesQuarantinesCorruptFiles(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	g.Expect(os.MkdirAll(s.deploymentsDir(), 0o700)).To(Succeed())

	healthyID := uuid.New()
	g.Expect(s.SaveState(DeploymentState{ID: healthyID, RevisionID: uuid.New(), State: StateReady})).
		To(Succeed())

	corruptID := uuid.New()
	g.Expect(os.WriteFile(s.stateFile(corruptID), []byte("{not json"), 0o600)).To(Succeed())
	// A truncated temp file from an interrupted write is ignored outright.
	g.Expect(os.WriteFile(filepath.Join(s.deploymentsDir(), ".uuid.tmp-123"), []byte("{"), 0o600)).To(Succeed())

	states, corruptions, err := s.LoadStates()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(states).To(HaveLen(1))
	g.Expect(states).To(HaveKey(healthyID))
	g.Expect(corruptions).To(HaveLen(1))
	g.Expect(corruptions[0].DeploymentID).To(Equal(corruptID))
	g.Expect(corruptions[0].Quarantined).NotTo(BeEmpty())
	g.Expect(corruptions[0].Quarantined).To(BeARegularFile())

	// The quarantined file is gone from the active directory, the healthy deployment keeps working.
	_, err = os.Stat(s.stateFile(corruptID))
	g.Expect(os.IsNotExist(err)).To(BeTrue())

	states, corruptions, err = s.LoadStates()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(states).To(HaveLen(1))
	g.Expect(corruptions).To(BeEmpty())
}

func TestCorruptGenerationQuarantinedAndClaimStillWorks(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	target := target()

	g.Expect(os.MkdirAll(s.gensDir(target.DeploymentID), 0o700)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(s.gensDir(target.DeploymentID), "broken.json"), []byte("truncated"), 0o600)).
		To(Succeed())

	unlock, _, err := s.TryLock(target.DeploymentID)
	g.Expect(err).NotTo(HaveOccurred())
	defer unlock()

	gen, adopted, claimCorruptions, err := s.Claim(target)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(adopted).To(BeFalse())
	g.Expect(gen.ID).NotTo(Equal(uuid.Nil))
	g.Expect(claimCorruptions).To(HaveLen(1))
	g.Expect(claimCorruptions[0].Quarantined).NotTo(BeEmpty())
}

func TestPruneGenerationsKeepsNewestAndUnreported(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	target := target()
	unlock, _, err := s.TryLock(target.DeploymentID)
	g.Expect(err).NotTo(HaveOccurred())
	defer unlock()

	first, _ := mustClaim(g, s, target)
	first.Phase = PhaseFinalized
	first.Receipt = &Receipt{Outcome: OutcomeSucceeded, Reported: true}
	g.Expect(s.SaveGeneration(first)).To(Succeed())

	time.Sleep(10 * time.Millisecond)
	target.RevisionID = uuid.New()
	second, _ := mustClaim(g, s, target)

	// The first generation is pruned, the newest one and any unreported one are kept.
	g.Expect(s.PruneGenerations(target.DeploymentID)).To(Succeed())
	gens, _, err := s.LoadGenerations(target.DeploymentID)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(gens).To(HaveLen(1))
	g.Expect(gens[0].ID).To(Equal(second.ID))
}

func TestProjectFilesRejectedInStagingKeepOldRevision(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	id := uuid.New()

	targetDir := s.ProjectDir(id)
	g.Expect(os.MkdirAll(targetDir, 0o700)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(targetDir, "docker-compose.yaml"), []byte("name: old\n"), 0o600)).
		To(Succeed())

	staging, err := s.StagingDir(id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(WriteStagedFile(staging, "docker-compose.yaml", []byte("name: new\n"))).To(Succeed())

	// Content fails validation in staging and is discarded without touching the published directory.
	g.Expect(RemoveStagingDir(staging)).To(Succeed())
	content, err := os.ReadFile(filepath.Join(targetDir, "docker-compose.yaml"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(content)).To(Equal("name: old\n"))
}

func TestProjectFileCommitAndRemove(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	id := uuid.New()

	targetDir := s.ProjectDir(id)
	g.Expect(os.MkdirAll(targetDir, 0o700)).To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(targetDir, "docker-compose.yaml"), []byte("name: old\n"), 0o600)).
		To(Succeed())
	g.Expect(os.WriteFile(filepath.Join(targetDir, ".env"), []byte("A=1\n"), 0o600)).To(Succeed())

	staging, err := s.StagingDir(id)
	g.Expect(err).NotTo(HaveOccurred())

	// Env content is replaced first and compose content last, so a crash never pairs new compose with
	// old env.
	g.Expect(WriteStagedFile(staging, ".env", []byte("A=2\n"))).To(Succeed())
	g.Expect(s.CommitProjectFile(id, staging, ".env")).To(Succeed())
	g.Expect(WriteStagedFile(staging, "docker-compose.yaml", []byte("name: new\n"))).To(Succeed())
	g.Expect(s.CommitProjectFile(id, staging, "docker-compose.yaml")).To(Succeed())

	content, err := os.ReadFile(filepath.Join(targetDir, "docker-compose.yaml"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(content)).To(Equal("name: new\n"))
	content, err = os.ReadFile(filepath.Join(targetDir, ".env"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(content)).To(Equal("A=2\n"))

	g.Expect(s.RemoveProjectFile(id, staging, ".env")).To(Succeed())
	_, err = os.Stat(filepath.Join(targetDir, ".env"))
	g.Expect(os.IsNotExist(err)).To(BeTrue())

	g.Expect(RemoveStagingDir(staging)).To(Succeed())
	_, err = os.Stat(staging)
	g.Expect(os.IsNotExist(err)).To(BeTrue())
}

func TestSweepIncompleteRemovesStaging(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)

	id := uuid.New()
	g.Expect(os.MkdirAll(s.gensDir(id), 0o700)).To(Succeed())
	g.Expect(os.MkdirAll(s.projectsDir(), 0o700)).To(Succeed())
	g.Expect(os.MkdirAll(s.ProjectDir(id), 0o700)).To(Succeed())

	staleState := filepath.Join(s.deploymentsDir(), ".abc.tmp-000")
	g.Expect(os.WriteFile(staleState, []byte("x"), 0o600)).To(Succeed())
	staleGen := filepath.Join(s.gensDir(id), ".gen.tmp-000")
	g.Expect(os.WriteFile(staleGen, []byte("x"), 0o600)).To(Succeed())
	staging, err := s.StagingDir(id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(os.WriteFile(filepath.Join(s.ProjectDir(id), ".docker-compose.yaml.tmp-1"), []byte("x"), 0o600)).
		To(Succeed())

	g.Expect(s.SweepIncomplete()).To(Succeed())

	for _, path := range []string{staleState, staleGen, staging,
		filepath.Join(s.ProjectDir(id), ".docker-compose.yaml.tmp-1")} {
		_, err := os.Stat(path)
		g.Expect(os.IsNotExist(err)).To(BeTrue(), path)
	}
}

func TestProjectDirBlockingFileQuarantined(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	id := uuid.New()

	// A regular file sits where the project directory is expected, as a crashed write could leave it.
	g.Expect(os.MkdirAll(s.projectsDir(), 0o700)).To(Succeed())
	blocker := s.ProjectDir(id)
	g.Expect(os.WriteFile(blocker, []byte("not a directory"), 0o600)).To(Succeed())

	staging, err := s.StagingDir(id)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(WriteStagedFile(staging, "docker-compose.yaml", []byte("name: example\n"))).To(Succeed())
	g.Expect(s.CommitProjectFile(id, staging, "docker-compose.yaml")).To(Succeed())

	content, err := os.ReadFile(filepath.Join(s.ProjectDir(id), "docker-compose.yaml"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(string(content)).To(Equal("name: example\n"))

	entries, err := os.ReadDir(s.quarantineDir())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(entries).To(HaveLen(1))
	g.Expect(entries[0].Name()).To(ContainSubstring(id.String()))
}

func TestLegacyStateWithoutGenerationReads(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	id := uuid.New()
	revision := uuid.New()
	g.Expect(os.MkdirAll(s.deploymentsDir(), 0o700)).To(Succeed())
	// Wire format written by controllers before generations existed.
	g.Expect(os.WriteFile(s.stateFile(id), []byte(
		`{"id":"`+id.String()+`","revisionId":"`+revision.String()+`",`+
			`"projectName":"example","docker_type":"compose","phase":"ready"}`), 0o600)).To(Succeed())

	states, corruptions, err := s.LoadStates()
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(corruptions).To(BeEmpty())
	st := states[id]
	g.Expect(st.AppliedRevisionID()).To(Equal(revision))
	g.Expect(st.GenerationID).To(Equal(uuid.Nil))
}

func TestStatePublishedAtomicallyIsNeverPartial(t *testing.T) {
	g := NewWithT(t)
	s := newTestStore(t)
	target := target()
	unlock, _, err := s.TryLock(target.DeploymentID)
	g.Expect(err).NotTo(HaveOccurred())
	defer unlock()
	gen, _ := mustClaim(g, s, target)

	st := DeploymentState{
		ID: target.DeploymentID, RevisionID: target.RevisionID,
		GenerationID: gen.ID, State: StateProgressing, ProjectName: "example",
	}
	g.Expect(s.PublishState(st)).To(Succeed())
	data, err := os.ReadFile(s.stateFile(target.DeploymentID))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(data).To(ContainSubstring(`"phase": "progressing"`))
	entries, err := os.ReadDir(s.deploymentsDir())
	g.Expect(err).NotTo(HaveOccurred())
	for _, e := range entries {
		g.Expect(isTempName(e.Name())).To(BeFalse(), e.Name())
	}
}
