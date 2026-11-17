package dialog

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/docker/docker-agent/pkg/chat"
	"github.com/docker/docker-agent/pkg/session"
)

func TestCostDialogAuxiliaryUsageIsAccountingNotMessage(t *testing.T) {
	sess := session.New()
	sess.AddAuxiliaryUsage(session.AuxiliaryUsage{Agent: "root", Model: "test/warm", Cost: 0.25, Usage: chat.Usage{InputTokens: 40, OutputTokens: 2}})
	sess.AddMessageUsageRecord("root", "test/real", 0.5, &chat.Usage{InputTokens: 80, OutputTokens: 4})
	d := NewCostDialog(sess).(*costDialog)
	for range 2 {
		data := d.gatherCostData()
		assert.Equal(t, 0.75, data.total.cost)
		assert.Equal(t, int64(120), data.total.InputTokens)
		require.Len(t, data.models, 2)
		require.Len(t, data.messages, 1, "auxiliary requests are not transcript rows")
		assert.Equal(t, "test/real", data.messages[0].model)
	}
}
