package controllerapply

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestCanonicalImageRef(t *testing.T) {
	g := NewWithT(t)
	cases := map[string]string{
		"nginx":                                  "nginx:latest",
		"nginx:1.27":                             "nginx:1.27",
		"registry.example.com/app:v1":            "registry.example.com/app:v1",
		"registry.example.com:5000/app":          "registry.example.com:5000/app:latest",
		"registry.example.com:5000/app:v2":       "registry.example.com:5000/app:v2",
		"nginx@sha256:abcdef":                    "nginx:latest",
		"registry.example.com/app:v1@sha256:abcd": "registry.example.com/app:v1",
		"  Nginx:1.27  ":                         "nginx:1.27",
		"": "",
	}
	for input, expected := range cases {
		g.Expect(CanonicalImageRef(input)).To(Equal(expected), input)
	}
}

func TestObsoleteImages(t *testing.T) {
	g := NewWithT(t)
	previous := []string{
		"registry.example.com/app:v1",
		"nginx:1.26",
		"nginx:1.26@sha256:olddigest",
		"redis:7",
	}
	current := []string{
		"registry.example.com/app:v2",
		"nginx:1.26",
		"redis:7@sha256:newdigest",
	}
	obsolete := ObsoleteImages(previous, current)
	g.Expect(obsolete).To(ConsistOf("registry.example.com/app:v1"))
}
