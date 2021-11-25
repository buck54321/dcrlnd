package dcrwallet

type SyncManager interface {
	Start(onSynced func(bool)) error
	Stop()
	WaitForShutdown()
}

type managedSyncer struct {
	m SyncManager
}

var _ WalletSyncer = (*managedSyncer)(nil)

func NewManagedSyncer(m SyncManager) WalletSyncer {
	return &managedSyncer{m}
}

func (s *managedSyncer) start(w *DcrWallet) error {
	return s.m.Start(w.onSyncerSynced)
}

func (s *managedSyncer) stop() {
	s.m.Stop()
}

func (s *managedSyncer) waitForShutdown() {
	s.m.WaitForShutdown()
}
