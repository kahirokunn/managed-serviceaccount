package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestConfigCheckerPaths(t *testing.T) {
	t.Setenv("HUB_KUBECONFIG", "/etc/hub/kubeconfig")

	cases := []struct {
		name            string
		spokeKubeconfig string
		want            []string
	}{
		{
			name: "default mode watches only the hub kubeconfig",
			want: []string{"/etc/hub/kubeconfig"},
		},
		{
			name:            "hosted mode also watches the rotating managed kubeconfig",
			spokeKubeconfig: "/etc/managed/kubeconfig",
			want:            []string{"/etc/hub/kubeconfig", "/etc/managed/kubeconfig"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			opts := &AgentOptions{SpokeKubeconfig: c.spokeKubeconfig}
			assert.Equal(t, c.want, opts.configCheckerPaths())
		})
	}
}
