package root

import (
	"testing"

	"github.com/docker/cli/cli-plugins/metadata"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

func TestStripPluginNameFromCompletionArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "strips agent token from __complete invocation",
			args: []string{cobra.ShellCompRequestCmd, "agent", "run", ""},
			want: []string{cobra.ShellCompRequestCmd, "run", ""},
		},
		{
			name: "strips agent token from __completeNoDesc invocation",
			args: []string{cobra.ShellCompNoDescRequestCmd, "agent", "run", ""},
			want: []string{cobra.ShellCompNoDescRequestCmd, "run", ""},
		},
		{
			name: "leaves normal run args unchanged",
			args: []string{"run", "agent.yaml"},
			want: []string{"run", "agent.yaml"},
		},
		{
			name: "leaves standalone complete args unchanged",
			args: []string{cobra.ShellCompRequestCmd, "run", ""},
			want: []string{cobra.ShellCompRequestCmd, "run", ""},
		},
		{
			name: "does not strip non-agent second token",
			args: []string{cobra.ShellCompRequestCmd, "other", "run", ""},
			want: []string{cobra.ShellCompRequestCmd, "other", "run", ""},
		},
		{
			name: "strips agent token with no trailing args (len==2)",
			args: []string{cobra.ShellCompRequestCmd, "agent"},
			want: []string{cobra.ShellCompRequestCmd},
		},
		{
			name: "handles empty args",
			args: []string{},
			want: []string{},
		},
		{
			name: "handles single arg",
			args: []string{cobra.ShellCompRequestCmd},
			want: []string{cobra.ShellCompRequestCmd},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := stripPluginNameFromCompletionArgs(tt.args)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsManagementInvocation(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{
		{metadata.MetadataSubcommandName},
		{cobra.ShellCompRequestCmd},
		{cobra.ShellCompNoDescRequestCmd},
		{"completion", "bash"},
		{"version"},
		{"help"},
		{"--help"},
		{"-h"},
		{"--version"},
		{"run", "--help"},
		{"run", "agent.yaml", "-h"},
		{"share", "push", "--help"},
	} {
		assert.True(t, isManagementInvocation(args), "args %v", args)
	}

	for _, args := range [][]string{
		nil,
		{},
		{"run", "agent.yaml"},
		{"new"},
	} {
		assert.False(t, isManagementInvocation(args), "args %v", args)
	}
}
