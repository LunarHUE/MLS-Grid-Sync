package git

import "errors"

var (
	// ErrNothingToCommit is returned by Writer.Commit when the working tree
	// has no staged or unstaged changes.
	ErrNothingToCommit = errors.New("nothing to commit")

	// ErrPushRejected is returned by Writer.Push when the remote rejects the
	// push (typically a non-fast-forward due to concurrent commits).
	ErrPushRejected = errors.New("push rejected (non-fast-forward)")

	// ErrPollTimeout is returned by Reader.PollForFile when the context is
	// cancelled before the file appears.
	ErrPollTimeout = errors.New("poll timed out waiting for file")
)
