package controllerapply

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrGenerationSuperseded means a newer generation exists and the caller's state or receipt must not
	// be published.
	ErrGenerationSuperseded = errors.New("application generation has been superseded")
	// ErrNoGeneration means a generation-gated write was attempted without any claimed generation, which
	// happens when a deployment has been uninstalled.
	ErrNoGeneration = errors.New("no application generation exists")
)

// Corruption describes a state or generation file that could not be decoded and was moved to the
// quarantine directory instead of blocking every deployment.
type Corruption struct {
	Kind         string
	Path         string
	DeploymentID uuid.UUID
	Err          error
	Quarantined  string
}

// Store is the on-disk application state rooted at the controller scratch directory.
type Store struct {
	root string
}

func New(root string) *Store {
	return &Store{root: root}
}

func (s *Store) Root() string { return s.root }

func (s *Store) deploymentsDir() string { return filepath.Join(s.root, "deployments") }
func (s *Store) quarantineDir() string  { return filepath.Join(s.deploymentsDir(), "quarantine") }
func (s *Store) locksDir() string       { return filepath.Join(s.deploymentsDir(), "locks") }

func (s *Store) lockFile(id uuid.UUID) string {
	return filepath.Join(s.locksDir(), id.String()+".lock")
}

func (s *Store) stateFile(id uuid.UUID) string {
	return filepath.Join(s.deploymentsDir(), id.String())
}
func (s *Store) gensRootDir() string         { return filepath.Join(s.deploymentsDir(), "gens") }
func (s *Store) gensDir(id uuid.UUID) string { return filepath.Join(s.gensRootDir(), id.String()) }

func (s *Store) genFile(deploymentID, id uuid.UUID) string {
	return filepath.Join(s.gensDir(deploymentID), id.String()+".json")
}

func (s *Store) projectsDir() string            { return filepath.Join(s.root, "projects") }
func (s *Store) ProjectDir(id uuid.UUID) string { return filepath.Join(s.projectsDir(), id.String()) }

func (s *Store) selfUpdateDir() string { return filepath.Join(s.root, "self-update") }

// ensureDir creates dir with 0700 permissions. A regular file sitting where a directory is required
// gets quarantined once instead of breaking every later write.
func (s *Store) ensureDir(dir string) error {
	err := os.MkdirAll(dir, 0o700)
	if err == nil {
		return nil
	}
	if !strings.Contains(err.Error(), "not a directory") {
		return err
	}

	blocker, ok := s.findBlockingFile(dir)
	if !ok {
		return err
	}
	if err := os.MkdirAll(s.quarantineDir(), 0o700); err != nil {
		return err
	}
	dest := filepath.Join(s.quarantineDir(),
		time.Now().UTC().Format("20060102T150405.000000000")+"-"+filepath.Base(blocker))
	if renameErr := os.Rename(blocker, dest); renameErr != nil {
		return errors.Join(err, renameErr)
	}
	return os.MkdirAll(dir, 0o700)
}

// findBlockingFile walks from the store root toward dir returning the first path component that exists
// but is not a directory.
func (s *Store) findBlockingFile(dir string) (string, bool) {
	rel, err := filepath.Rel(s.root, dir)
	if err != nil {
		return "", false
	}
	current := s.root
	for _, part := range strings.Split(rel, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		next := filepath.Join(current, part)
		info, err := os.Lstat(next)
		if err != nil {
			return "", false
		}
		if !info.IsDir() {
			return next, true
		}
		current = next
	}
	return "", false
}

// quarantineFile moves an unreadable file into the quarantine directory. If the move fails the file
// stays in place and Corruption.Quarantined stays empty.
func (s *Store) quarantineFile(kind, path string, deploymentID uuid.UUID, cause error) Corruption {
	c := Corruption{Kind: kind, Path: path, DeploymentID: deploymentID, Err: cause}
	if err := os.MkdirAll(s.quarantineDir(), 0o700); err == nil {
		name := filepath.Base(path)
		if deploymentID != uuid.Nil {
			name = deploymentID.String() + "-" + name
		}
		dest := filepath.Join(s.quarantineDir(),
			time.Now().UTC().Format("20060102T150405.000000000")+"-"+name)
		if err := os.Rename(path, dest); err == nil {
			c.Quarantined = dest
		}
	}
	return c
}

// LoadStates returns all readable deployment states. One corrupt file is quarantined and reported but
// does not stop the remaining deployments from loading.
func (s *Store) LoadStates() (map[uuid.UUID]DeploymentState, []Corruption, error) {
	entries, err := os.ReadDir(s.deploymentsDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[uuid.UUID]DeploymentState{}, nil, nil
		}
		return nil, nil, err
	}

	result := map[uuid.UUID]DeploymentState{}
	var corruptions []Corruption
	for _, entry := range entries {
		if entry.IsDir() || isTempName(entry.Name()) {
			continue
		}
		fpath := filepath.Join(s.deploymentsDir(), entry.Name())
		var st DeploymentState
		if err := readJSONFile(fpath, &st); err != nil {
			id := uuid.Nil
			if parsed, parseErr := uuid.Parse(entry.Name()); parseErr == nil {
				id = parsed
			}
			corruptions = append(corruptions, s.quarantineFile("deployment state", fpath, id, err))
			continue
		}
		result[st.ID] = st
	}
	return result, corruptions, nil
}

func readJSONFile(fpath string, value any) error {
	data, err := os.ReadFile(fpath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return err
	}
	return nil
}

// SaveState publishes state without a generation check. It is only used for legacy records without a
// generation; generation-owned state goes through PublishState.
func (s *Store) SaveState(st DeploymentState) error {
	if err := s.ensureDir(s.deploymentsDir()); err != nil {
		return err
	}
	return writeJSONAtomic(s.stateFile(st.ID), st)
}

// PublishState publishes state only while the given generation is still the newest one for the
// deployment. A generation that lost to a newer revision or to an uninstall cannot overwrite state.
func (s *Store) PublishState(st DeploymentState) error {
	if st.GenerationID == uuid.Nil {
		return s.SaveState(st)
	}
	current, ok, err := s.newestGeneration(st.ID)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoGeneration
	}
	if current.ID != st.GenerationID {
		return ErrGenerationSuperseded
	}
	return writeJSONAtomic(s.stateFile(st.ID), st)
}

func (s *Store) DeleteState(id uuid.UUID) error {
	if err := os.Remove(s.stateFile(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Claim returns the live generation for the target revision, adopting one a crashed or replaced
// process left behind, or claims a new unique generation. Corrupt generation records encountered are
// quarantined and returned for reporting. Claim has to be called while holding the deployment lock so
// two processes cannot claim the same revision concurrently.
func (s *Store) Claim(target Target) (*Generation, bool, []Corruption, error) {
	if err := s.ensureDir(s.gensDir(target.DeploymentID)); err != nil {
		return nil, false, nil, err
	}

	gens, corruptions, err := s.loadGenerations(target.DeploymentID)
	if err != nil {
		return nil, false, corruptions, err
	}
	sort.Slice(gens, func(i, j int) bool { return gens[i].ClaimedAt.After(gens[j].ClaimedAt) })
	for _, g := range gens {
		if g.RevisionID == target.RevisionID && g.isLive() {
			return &g, true, corruptions, nil
		}
	}

	gen := &Generation{
		ID:           uuid.New(),
		DeploymentID: target.DeploymentID,
		RevisionID:   target.RevisionID,
		ProjectName:  target.ProjectName,
		DockerType:   target.DockerType,
		Phase:        PhaseClaimed,
		ClaimedAt:    time.Now().UTC(),
	}
	if err := s.persistGeneration(gen); err != nil {
		return nil, false, corruptions, err
	}
	return gen, false, corruptions, nil
}

// SaveGeneration persists a generation only while it is still the newest one for its deployment.
func (s *Store) SaveGeneration(g *Generation) error {
	current, ok, err := s.newestGeneration(g.DeploymentID)
	if err != nil {
		return err
	}
	if !ok || current.ID != g.ID {
		return ErrGenerationSuperseded
	}
	return s.persistGeneration(g)
}

func (s *Store) persistGeneration(g *Generation) error {
	return writeJSONAtomic(s.genFile(g.DeploymentID, g.ID), g)
}

// newestGeneration returns the generation with the latest claim time. Superseded generations stay on
// disk for receipts but can never publish again.
func (s *Store) newestGeneration(deploymentID uuid.UUID) (*Generation, bool, error) {
	gens, _, err := s.loadGenerations(deploymentID)
	if err != nil {
		return nil, false, err
	}
	if len(gens) == 0 {
		return nil, false, nil
	}
	newest := slices.MaxFunc(gens, func(a, b Generation) int {
		return a.ClaimedAt.Compare(b.ClaimedAt)
	})
	return &newest, true, nil
}

func (s *Store) loadGenerations(deploymentID uuid.UUID) ([]Generation, []Corruption, error) {
	dir := s.gensDir(deploymentID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var gens []Generation
	var corruptions []Corruption
	for _, entry := range entries {
		if entry.IsDir() || isTempName(entry.Name()) {
			continue
		}
		fpath := filepath.Join(dir, entry.Name())
		var g Generation
		if err := readJSONFile(fpath, &g); err != nil {
			corruptions = append(corruptions, s.quarantineFile("generation", fpath, deploymentID, err))
			continue
		}
		if g.ID == uuid.Nil || g.DeploymentID == uuid.Nil {
			corruptions = append(corruptions, s.quarantineFile("generation", fpath, deploymentID,
				errors.New("generation record is missing ids")))
			continue
		}
		gens = append(gens, g)
	}
	return gens, corruptions, nil
}

// LoadGenerations returns the generations of one deployment, quarantining corrupt records.
func (s *Store) LoadGenerations(deploymentID uuid.UUID) ([]Generation, []Corruption, error) {
	return s.loadGenerations(deploymentID)
}

// PendingGenerations returns finalized generations whose final server callback was never confirmed,
// across every deployment, so a restarted controller can re-report from their receipts.
func (s *Store) PendingGenerations() ([]*Generation, error) {
	deployments, err := os.ReadDir(s.gensRootDir())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var pending []*Generation
	for _, depEntry := range deployments {
		if !depEntry.IsDir() {
			continue
		}
		id, err := uuid.Parse(depEntry.Name())
		if err != nil {
			continue
		}
		gens, _, err := s.loadGenerations(id)
		if err != nil {
			return nil, err
		}
		for i := range gens {
			g := gens[i]
			if g.Phase == PhaseFinalized && g.Receipt != nil && !g.Receipt.Reported {
				pending = append(pending, &g)
			}
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].ClaimedAt.Before(pending[j].ClaimedAt) })
	return pending, nil
}

// PruneGenerations deletes finalized and reported generations of a deployment except its newest one, so
// receipts survive one callback retry cycle without accumulating forever.
func (s *Store) PruneGenerations(deploymentID uuid.UUID) error {
	gens, _, err := s.loadGenerations(deploymentID)
	if err != nil {
		return err
	}
	if len(gens) <= 1 {
		return nil
	}
	newest, _, err := s.newestGeneration(deploymentID)
	if err != nil {
		return err
	}
	for _, g := range gens {
		if newest != nil && g.ID == newest.ID {
			continue
		}
		if g.Phase != PhaseFinalized || g.Receipt == nil || !g.Receipt.Reported {
			continue
		}
		if err := os.Remove(s.genFile(deploymentID, g.ID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// DeleteGenerations removes every generation record of a deployment as part of its uninstall.
func (s *Store) DeleteGenerations(deploymentID uuid.UUID) error {
	dir := s.gensDir(deploymentID)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncDir(s.gensRootDir())
}

// SaveSelfUpdateReceipt persists the result of a detached engine action, namely a controller
// self-update, independently of any deployment.
func (s *Store) SaveSelfUpdateReceipt(r Receipt) error {
	if err := s.ensureDir(s.selfUpdateDir()); err != nil {
		return err
	}
	return writeJSONAtomic(filepath.Join(s.selfUpdateDir(), "last.json"), r)
}

// SweepIncomplete removes unpublished staging files and directories left by interrupted writes in the
// deployments and projects trees. Published files are never touched.
func (s *Store) SweepIncomplete() error {
	return errors.Join(
		sweepTempChildren(s.deploymentsDir()),
		sweepProjectStaging(s.projectsDir()),
	)
}

func sweepTempChildren(dir string) error {
	var walk func(root string, depth int) []error
	walk = func(root string, depth int) []error {
		entries, err := os.ReadDir(root)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return []error{err}
		}
		var errs []error
		for _, entry := range entries {
			fpath := filepath.Join(root, entry.Name())
			if isTempName(entry.Name()) {
				if err := os.RemoveAll(fpath); err != nil {
					errs = append(errs, err)
				}
				continue
			}
			if depth > 0 && entry.IsDir() {
				errs = append(errs, walk(fpath, depth-1)...)
			}
		}
		return errs
	}
	return errors.Join(walk(dir, 2)...)
}

func sweepProjectStaging(projectsDir string) error {
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	var errs []error
	for _, entry := range entries {
		fpath := filepath.Join(projectsDir, entry.Name())
		if isTempName(entry.Name()) {
			if err := os.RemoveAll(fpath); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if !entry.IsDir() {
			continue
		}
		children, err := os.ReadDir(fpath)
		if err != nil {
			continue
		}
		for _, child := range children {
			if isTempName(child.Name()) {
				if err := os.RemoveAll(filepath.Join(fpath, child.Name())); err != nil {
					errs = append(errs, err)
				}
			}
		}
	}
	return errors.Join(errs...)
}
