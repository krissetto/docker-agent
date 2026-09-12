package runtime

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestObservationPrimary(t *testing.T) {
	primary := SessionSnapshot{Cursor: 7}
	observation := Observation{Initial: []SessionSnapshot{primary, {Cursor: 8}}}
	assert.Equal(t, primary, observation.Primary())
	assert.Equal(t, SessionSnapshot{}, (Observation{}).Primary())
}
