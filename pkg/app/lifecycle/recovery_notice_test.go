package lifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestInterruptedTurnsNoticeDescribesMissingTerminalRecords(t *testing.T) {
	assert.Equal(t, "Recovery uncertain: 1 accepted input has no terminal outcome record. Review the transcript before continuing.", InterruptedTurnsNotice(1))
	assert.Equal(t, "Recovery uncertain: 299 accepted inputs have no terminal outcome records. Review the transcript before continuing.", InterruptedTurnsNotice(299))
}
