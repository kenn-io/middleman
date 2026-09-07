package localruntime

import (
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestAgentResumeCommand(t *testing.T) {
	for _, tc := range []struct {
		agent  string
		suffix []string
	}{
		{"codex", []string{"resume", "conversation"}},
		{"claude", []string{"--resume", "conversation"}},
		{"pi", []string{"--session", "conversation"}},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			command, err := agentResumeCommand([]string{tc.agent, "--model", "model-a"}, tc.agent, "conversation")
			require.NoError(t, err)
			assert.Equal(t, append([]string{tc.agent, "--model", "model-a"}, tc.suffix...), command)
		})
	}
	_, err := agentResumeCommand([]string{"other"}, "other", "conversation")
	require.Error(t, err)
}
