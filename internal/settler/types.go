package settler

// StopSignal carries the reason a sandbox should be stopped.
type StopSignal struct {
	SandboxID string
	Reason    string // "insufficient_balance" | "not_acknowledged"
}
