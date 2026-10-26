package worktree

// TryLockRetirement fails closed on Windows until the retirement transaction's
// nonblocking exclusion and filesystem guarantees are supported together.
func TryLockRetirement(_, _ string) (func(), error) { return nil, ErrRetirementLockUnsupported }
