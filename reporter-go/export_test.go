package reporter

// NewTestNotifier returns a notifier with a client of its own, configured
// only by cfg: environment variables are ignored and, unless cfg sets one, no
// PanicHandler runs.
func NewTestNotifier(cfg Configuration) *Notifier {
	c := newClient()
	c.getenv = func(string) string { return "" }
	if cfg.PanicHandler == nil {
		cfg.PanicHandler = func() {}
	}
	c.configure(cfg)
	return &Notifier{client: c}
}

// WithRawData returns a notifier like New's, sharing n's client.
func WithRawData(n *Notifier, rawData ...any) *Notifier {
	return &Notifier{client: n.client, rawData: rawData}
}

// StatsOf returns the counts of n's client.
func StatsOf(n *Notifier) Counts {
	return n.client.counts()
}

// Drain stops n's queue and waits until every queued event has been handled.
func Drain(n *Notifier) {
	n.client.pub.close()
	<-n.client.pub.done
}
