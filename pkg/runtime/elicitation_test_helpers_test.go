package runtime

func elicitationWaiterCountForTest(waiters *elicitationWaiters) int {
	waiters.mu.Lock()
	defer waiters.mu.Unlock()
	return len(waiters.pending)
}
