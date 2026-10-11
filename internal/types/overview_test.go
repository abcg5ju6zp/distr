package types

import (
	"testing"
	"time"

	. "github.com/onsi/gomega"
)

func TestLatestEntitledApplicationVersionOrdersSubsetByWholeSet(t *testing.T) {
	g := NewWithT(t)
	base := time.Now()
	v2 := version("2.0.0", base)
	v1 := version("1.0.0", base.Add(time.Minute))
	nightly := version("nightly", base.Add(2*time.Minute))
	application := Application{
		VersioningStrategy: VersioningStrategyLegacy,
		Versions:           []ApplicationVersion{v2, v1, nightly},
	}

	// The unparseable name outside the entitlement still makes the legacy strategy order by date.
	entitlement := &ApplicationEntitlementWithVersions{Versions: []ApplicationVersion{v2, v1}}
	latest := LatestEntitledApplicationVersion(application, entitlement)
	g.Expect(latest).NotTo(BeNil())
	g.Expect(latest.ID).To(Equal(v1.ID))

	latest = LatestEntitledApplicationVersion(application, &ApplicationEntitlementWithVersions{})
	g.Expect(latest).NotTo(BeNil())
	g.Expect(latest.ID).To(Equal(nightly.ID), "an entitlement without versions covers every version")
}

func TestApplicationUpdateAvailable(t *testing.T) {
	g := NewWithT(t)
	base := time.Now()
	v1 := version("1.0.0", base)
	v2 := version("2.0.0", base.Add(time.Minute))
	v3 := version("3.0.0", base.Add(2*time.Minute))
	application := Application{
		VersioningStrategy: VersioningStrategySemver,
		Versions:           []ApplicationVersion{v1, v2, v3},
	}

	g.Expect(ApplicationUpdateAvailable(application, v1.ID, &v2)).To(BeTrue())
	g.Expect(ApplicationUpdateAvailable(application, v2.ID, &v2)).To(BeFalse())
	g.Expect(ApplicationUpdateAvailable(application, v3.ID, &v2)).
		To(BeFalse(), "a deployment ahead of the latest entitled version has no update")
	g.Expect(ApplicationUpdateAvailable(application, v1.ID, nil)).To(BeFalse())
}

func TestAccessTokenNextExpiresAt(t *testing.T) {
	g := NewWithT(t)
	now := time.Now()
	past := now.Add(-48 * time.Hour)
	recently := now.Add(-time.Hour)
	soon := now.Add(time.Hour)
	later := now.Add(48 * time.Hour)

	g.Expect(AccessToken{
		Secret1: &AccessTokenSecret{ExpiresAt: &later},
		Secret2: &AccessTokenSecret{ExpiresAt: &soon},
	}.NextExpiresAt(now)).To(Equal(&soon), "a secret expiring soon is reported although another one outlives it")

	g.Expect(AccessToken{
		Secret1: &AccessTokenSecret{ExpiresAt: &soon},
		Secret2: &AccessTokenSecret{},
	}.NextExpiresAt(now)).To(Equal(&soon), "a secret without expiration does not hide one that expires")

	g.Expect(AccessToken{
		Secret1: &AccessTokenSecret{ExpiresAt: &past},
		Secret2: &AccessTokenSecret{ExpiresAt: &later},
	}.NextExpiresAt(now)).To(Equal(&later), "a secret replaced by a rotation is ignored")

	g.Expect(AccessToken{
		Secret1: &AccessTokenSecret{ExpiresAt: &past},
		Secret2: &AccessTokenSecret{},
	}.NextExpiresAt(now)).To(BeNil())

	g.Expect(AccessToken{
		Secret1: &AccessTokenSecret{ExpiresAt: &past},
		Secret2: &AccessTokenSecret{ExpiresAt: &recently},
	}.NextExpiresAt(now)).To(Equal(&recently), "an expired token reports when its last credential expired")

	g.Expect(AccessToken{
		ExpiresAt: &soon,
		Secret1:   &AccessTokenSecret{ExpiresAt: &later},
	}.NextExpiresAt(now)).To(Equal(&later), "the expiration of a key that is no credential is ignored")
}
