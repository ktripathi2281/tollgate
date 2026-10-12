package budget

import "time"

// SetRetryWait shortens the pause between settle attempts, for tests.
func SetRetryWait(b *Budgets, d time.Duration) {
	b.retryWait = d
}
