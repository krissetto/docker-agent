package session

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPublishMessageIDPreservesObservedReceiptAndRejectsStaleIdentity(t *testing.T) {
	s := New()
	first, second := UserMessage("identical"), UserMessage("identical")
	s.AddMessage(first)
	s.AddMessage(second)
	observed := s.GetAllMessages()
	require.True(t, s.PublishMessageID(second, 92))
	require.True(t, s.PublishMessageID(first, 91))
	assert.Zero(t, first.ID)
	assert.Zero(t, second.ID)
	assert.Zero(t, observed[0].ID)
	assert.Zero(t, observed[1].ID)
	current := s.GetAllMessages()
	assert.Equal(t, int64(91), current[0].ID)
	assert.Equal(t, int64(92), current[1].ID)
	assert.False(t, s.PublishMessageID(first, 99))
	require.Equal(t, int64(91), s.GetAllMessages()[0].ID)
	cloned := s.Clone()
	assert.False(t, cloned.PublishMessageID(s.MessagesSnapshot()[0].Message, 100))
}

func TestPublishMessageIDConcurrentSnapshots(t *testing.T) {
	s := New()
	const count = 256
	receipts := make([]*Message, count)
	for i := range receipts {
		receipts[i] = UserMessage("message")
		s.AddMessage(receipts[i])
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	workers.Go(func() {
		<-start
		for i, receipt := range receipts {
			s.PublishMessageID(receipt, int64(i+1))
		}
	})
	workers.Go(func() {
		<-start
		for range count {
			for _, message := range s.GetAllMessages() {
				assert.Equal(t, "message", message.Message.Content)
			}
			for _, receipt := range receipts {
				assert.Zero(t, receipt.ID)
			}
		}
	})
	close(start)
	workers.Wait()
	for i, message := range s.GetAllMessages() {
		assert.Equal(t, int64(i+1), message.ID)
	}
}
