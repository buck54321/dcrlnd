// Copyright (c) 2013-2017 The btcsuite developers
// Copyright (c) 2015-2019 The Decred developers
// Copyright (C) 2015-2017 The Lightning Network Developers

package dcrlnd

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"strings"
	"sync"
	"time"

	// Blank import to set up profiling HTTP handlers.
	_ "net/http/pprof"

	"gopkg.in/macaroon-bakery.v2/bakery"
	"gopkg.in/macaroon.v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"decred.org/dcrwallet/v2/wallet"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	proxy "github.com/grpc-ecosystem/grpc-gateway/runtime"

	"github.com/decred/dcrlnd/autopilot"
	"github.com/decred/dcrlnd/build"
	"github.com/decred/dcrlnd/cert"
	"github.com/decred/dcrlnd/chanacceptor"
	"github.com/decred/dcrlnd/channeldb"
	"github.com/decred/dcrlnd/keychain"
	"github.com/decred/dcrlnd/lncfg"
	"github.com/decred/dcrlnd/lnrpc"
	"github.com/decred/dcrlnd/lnwallet"
	"github.com/decred/dcrlnd/lnwallet/dcrwallet"
	walletloader "github.com/decred/dcrlnd/lnwallet/dcrwallet/loader"
	"github.com/decred/dcrlnd/macaroons"
	"github.com/decred/dcrlnd/signal"
	"github.com/decred/dcrlnd/tor"
	"github.com/decred/dcrlnd/walletunlocker"
	"github.com/decred/dcrlnd/watchtower"
	"github.com/decred/dcrlnd/watchtower/wtdb"
)

// WalletUnlockerAuthOptions returns a list of DialOptions that can be used to
// authenticate with the wallet unlocker service.
//
// NOTE: This should only be called after the WalletUnlocker listener has
// signaled it is ready.
func WalletUnlockerAuthOptions(cfg *Config) ([]grpc.DialOption, error) {
	creds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert: %v", err)
	}

	// Create a dial options array with the TLS credentials.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	return opts, nil
}

// AdminAuthOptions returns a list of DialOptions that can be used to
// authenticate with the RPC server with admin capabilities.
//
// NOTE: This should only be called after the RPCListener has signaled it is
// ready.
func AdminAuthOptions(cfg *Config) ([]grpc.DialOption, error) {
	creds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, fmt.Errorf("unable to read TLS cert: %v", err)
	}

	// Create a dial options array.
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(creds),
	}

	// Get the admin macaroon if macaroons are active.
	if !cfg.NoMacaroons {
		// Load the adming macaroon file.
		macBytes, err := ioutil.ReadFile(cfg.AdminMacPath)
		if err != nil {
			return nil, fmt.Errorf("unable to read macaroon "+
				"path (check the network setting!): %v", err)
		}

		mac := &macaroon.Macaroon{}
		if err = mac.UnmarshalBinary(macBytes); err != nil {
			return nil, fmt.Errorf("unable to decode macaroon: %v",
				err)
		}

		// Now we append the macaroon credentials to the dial options.
		cred := macaroons.NewMacaroonCredential(mac)
		opts = append(opts, grpc.WithPerRPCCredentials(cred))
	}

	return opts, nil
}

// GrpcRegistrar is an interface that must be satisfied by an external subserver
// that wants to be able to register its own gRPC server onto lnd's main
// grpc.Server instance.
type GrpcRegistrar interface {
	// RegisterGrpcSubserver is called for each net.Listener on which lnd
	// creates a grpc.Server instance. External subservers implementing this
	// method can then register their own gRPC server structs to the main
	// server instance.
	RegisterGrpcSubserver(*grpc.Server) error
}

// RestRegistrar is an interface that must be satisfied by an external subserver
// that wants to be able to register its own REST mux onto lnd's main
// proxy.ServeMux instance.
type RestRegistrar interface {
	// RegisterRestSubserver is called after lnd creates the main
	// proxy.ServeMux instance. External subservers implementing this method
	// can then register their own REST proxy stubs to the main server
	// instance.
	RegisterRestSubserver(context.Context, *proxy.ServeMux, string,
		[]grpc.DialOption) error
}

// RPCSubserverConfig is a struct that can be used to register an external
// subserver with the custom permissions that map to the gRPC server that is
// going to be registered with the GrpcRegistrar.
type RPCSubserverConfig struct {
	// Registrar is a callback that is invoked for each net.Listener on
	// which lnd creates a grpc.Server instance.
	Registrar GrpcRegistrar

	// Permissions is the permissions required for the external subserver.
	// It is a map between the full HTTP URI of each RPC and its required
	// macaroon permissions. If multiple action/entity tuples are specified
	// per URI, they are all required. See rpcserver.go for a list of valid
	// action and entity values.
	Permissions map[string][]bakery.Op

	// MacaroonValidator is a custom macaroon validator that should be used
	// instead of the default lnd validator. If specified, the custom
	// validator is used for all URIs specified in the above Permissions
	// map.
	MacaroonValidator macaroons.MacaroonValidator
}

// ListenerWithSignal is a net.Listener that has an additional Ready channel that
// will be closed when a server starts listening.
type ListenerWithSignal struct {
	net.Listener

	// Ready will be closed by the server listening on Listener.
	Ready chan struct{}

	// ExternalRPCSubserverCfg is optional and specifies the registration
	// callback and permissions to register external gRPC subservers.
	ExternalRPCSubserverCfg *RPCSubserverConfig

	// ExternalRestRegistrar is optional and specifies the registration
	// callback to register external REST subservers.
	ExternalRestRegistrar RestRegistrar
}

// ListenerCfg is a wrapper around custom listeners that can be passed to lnd
// when calling its main method.
type ListenerCfg struct {
	// WalletUnlocker can be set to the listener to use for the wallet
	// unlocker. If nil a regular network listener will be created.
	WalletUnlocker *ListenerWithSignal

	// RPCListener can be set to the listener to use for the RPC server. If
	// nil a regular network listener will be created.
	RPCListener *ListenerWithSignal
}

// rpcListeners is a function type used for closures that fetches a set of RPC
// listeners for the current configuration. If no custom listeners are present,
// this should return normal listeners from the RPC endpoints defined in the
// config. The second return value us a closure that will close the fetched
// listeners.
type rpcListeners func() ([]*ListenerWithSignal, func(), error)

// Main is the true entry point for lnd. It accepts a fully populated and
// validated main configuration struct and an optional listener config struct.
// This function starts all main system components then blocks until a signal
// is received on the shutdownChan at which point everything is shut down again.
func Main(cfg *Config, lisCfg ListenerCfg, shutdownChan <-chan struct{}) error {
	defer func() {
		ltndLog.Info("Shutdown complete")
		err := cfg.LogWriter.Close()
		if err != nil {
			ltndLog.Errorf("Could not close log rotator: %v", err)
		}
	}()

	// Show version at startup.
	ltndLog.Infof("Version: %s, build=%s, logging=%s",
		build.Version(), build.Deployment, build.LoggingType)

	// Read IPC messages from the read end of a pipe created and passed by the
	// parent process, if any.  When this pipe is closed, shutdown is
	// initialized.
	if cfg.PipeRx != nil {
		go serviceControlPipeRx(uintptr(*cfg.PipeRx))
	}
	if cfg.PipeTx != nil {
		go serviceControlPipeTx(uintptr(*cfg.PipeTx))
	} else {
		go drainOutgoingPipeMessages()
	}

	// We default to mainnet if none are specified.
	network := "mainnet"
	switch {
	case cfg.TestNet3:
		network = "testnet"

	case cfg.SimNet:
		network = "simnet"

	case cfg.RegTest:
		network = "regtest"
	}

	ltndLog.Infof("Active chain: %v (network=%v)",
		strings.Title(cfg.registeredChains.PrimaryChain().String()),
		network,
	)

	// Enable http profiling server if requested.
	if cfg.Profile != "" {
		go func() {
			listenAddr := net.JoinHostPort("", cfg.Profile)
			profileRedirect := http.RedirectHandler("/debug/pprof",
				http.StatusSeeOther)
			http.Handle("/", profileRedirect)
			fmt.Println(http.ListenAndServe(listenAddr, nil))
		}()
	}

	// Write cpu profile if requested.
	if cfg.CPUProfile != "" {
		f, err := os.Create(cfg.CPUProfile)
		if err != nil {
			err := fmt.Errorf("unable to create CPU profile: %v",
				err)
			ltndLog.Error(err)
			return err
		}
		pprof.StartCPUProfile(f)
		defer f.Close()
		defer pprof.StopCPUProfile()
	}

	// Write memory profile if requested.
	if cfg.MemProfile != "" {
		f, err := os.Create(cfg.MemProfile)
		if err != nil {
			err := fmt.Errorf("unable to create memory profile: %v",
				err)
			ltndLog.Error(err)
			return err
		}
		defer f.Close()
		defer pprof.WriteHeapProfile(f)
	}

	if cfg.DB.Backend != "bolt" && network == "mainnet" {
		err := fmt.Errorf("Only bbolt db backend has been tested in production " +
			"for dcrlnd.")
		ltndLog.Error(err)
		return err
	}

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	localChanDB, remoteChanDB, cleanUp, err := initializeDatabases(ctx, cfg)
	switch {
	case err == channeldb.ErrDryRunMigrationOK:
		ltndLog.Infof("%v, exiting", err)
		return nil
	case err != nil:
		return fmt.Errorf("unable to open databases: %v", err)
	}

	defer cleanUp()

	// Only process macaroons if --no-macaroons isn't set.
	tlsCfg, restCreds, restProxyDest, err := getTLSConfig(cfg)
	if err != nil {
		err := fmt.Errorf("unable to load TLS credentials: %v", err)
		ltndLog.Error(err)
		return err
	}

	serverCreds := credentials.NewTLS(tlsCfg)
	serverOpts := []grpc.ServerOption{grpc.Creds(serverCreds)}

	// For our REST dial options, we'll still use TLS, but also increase
	// the max message size that we'll decode to allow clients to hit
	// endpoints which return more data such as the DescribeGraph call.
	// We set this to 200MiB atm. Should be the same value as maxMsgRecvSize
	// in cmd/lncli/main.go.
	restDialOpts := []grpc.DialOption{
		grpc.WithTransportCredentials(*restCreds),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(1 * 1024 * 1024 * 200),
		),
	}

	var (
		walletInitParams WalletUnlockParams
		privateWalletPw  = lnwallet.DefaultPrivatePassphrase
		publicWalletPw   = lnwallet.DefaultPublicPassphrase
	)

	// If the user didn't request a seed, then we'll manually assume a
	// wallet birthday of now, as otherwise the seed would've specified
	// this information.
	walletInitParams.Birthday = time.Now()

	isRemoteWallet := cfg.Dcrwallet.GRPCHost != "" && cfg.Dcrwallet.CertPath != ""

	// getListeners is a closure that creates listeners from the
	// RPCListeners defined in the config. It also returns a cleanup
	// closure and the server options to use for the GRPC server.
	getListeners := func() ([]*ListenerWithSignal, func(), error) {
		var grpcListeners []*ListenerWithSignal
		for _, grpcEndpoint := range cfg.RPCListeners {
			// Start a gRPC server listening for HTTP/2
			// connections.
			lis, err := lncfg.ListenOnAddress(grpcEndpoint)
			if err != nil {
				ltndLog.Errorf("unable to listen on %s",
					grpcEndpoint)
				return nil, nil, err
			}
			grpcListeners = append(
				grpcListeners, &ListenerWithSignal{
					Listener: lis,
					Ready:    make(chan struct{}),
				})
		}

		cleanup := func() {
			for _, lis := range grpcListeners {
				lis.Close()
			}
		}
		return grpcListeners, cleanup, nil
	}

	// walletUnlockerListeners is a closure we'll hand to the wallet
	// unlocker, that will be called when it needs listeners for its GPRC
	// server.
	walletUnlockerListeners := func() ([]*ListenerWithSignal, func(),
		error) {

		// If we have chosen to start with a dedicated listener for the
		// wallet unlocker, we return it directly.
		if lisCfg.WalletUnlocker != nil {
			return []*ListenerWithSignal{lisCfg.WalletUnlocker},
				func() {}, nil
		}

		// Otherwise we'll return the regular listeners.
		return getListeners()
	}

	// We wait until the user provides a password over RPC. In case lnd is
	// started with the --noseedbackup flag, we use the default password
	// for wallet encryption.
	if !cfg.NoSeedBackup || isRemoteWallet {
		params, err := waitForWalletPassword(
			cfg, cfg.RESTListeners, serverOpts, restDialOpts,
			restProxyDest, tlsCfg, walletUnlockerListeners, remoteChanDB,
		)
		if err != nil {
			err := fmt.Errorf("unable to set up wallet password "+
				"listeners: %v", err)
			ltndLog.Error(err)
			return err
		}

		walletInitParams = *params
		privateWalletPw = walletInitParams.Password
		publicWalletPw = walletInitParams.Password

		if walletInitParams.RecoveryWindow > 0 {
			ltndLog.Infof("Wallet recovery mode enabled with "+
				"address lookahead of %d addresses",
				walletInitParams.RecoveryWindow)
		}
	}

	var macaroonService *macaroons.Service
	if !cfg.NoMacaroons {
		// Create the macaroon authentication/authorization service.
		macaroonService, err = macaroons.NewService(
			cfg.networkDir, "lnd", macaroons.IPLockChecker,
		)
		if err != nil {
			err := fmt.Errorf("unable to set up macaroon "+
				"authentication: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer macaroonService.Close()

		// Try to unlock the macaroon store with the private password.
		err = macaroonService.CreateUnlock(&privateWalletPw)
		if err != nil {
			err := fmt.Errorf("unable to unlock macaroons: %v", err)
			ltndLog.Error(err)
			return err
		}

		// Create macaroon files for dcrlncli to use if they don't exist.
		if !fileExists(cfg.AdminMacPath) && !fileExists(cfg.ReadMacPath) &&
			!fileExists(cfg.InvoiceMacPath) {

			err = genMacaroons(
				ctx, macaroonService, cfg.AdminMacPath,
				cfg.ReadMacPath, cfg.InvoiceMacPath,
			)
			if err != nil {
				err := fmt.Errorf("unable to create macaroons "+
					"%v", err)
				ltndLog.Error(err)
				return err
			}
		}
	}

	// With the information parsed from the configuration, create valid
	// instances of the pertinent interfaces required to operate the
	// Lightning Network Daemon.
	//
	// When we create the chain control, we need storage for the height
	// hints and also the wallet itself, for these two we want them to be
	// replicated, so we'll pass in the remote channel DB instance.
	activeChainControl, err := newChainControlFromConfig(
		cfg, localChanDB, remoteChanDB, privateWalletPw, publicWalletPw,
		walletInitParams.Birthday, walletInitParams.RecoveryWindow,
		walletInitParams.Wallet, walletInitParams.Loader,
		walletInitParams.Conn, cfg.Dcrwallet.AccountNumber,
	)
	if err != nil {
		err := fmt.Errorf("unable to create chain control: %v", err)
		ltndLog.Error(err)
		return err
	}

	// Wait until we're fully synced to continue the start up of the remainder
	// of the daemon. This ensures that we don't accept any possibly invalid
	// state transitions, or accept channels with spent funds.
	//
	// This is also required on decred due to various things dcrwallet does at
	// startup (mainly, slip0044 upgrade, account and address discovery) which
	// may deadlock if we try to start using it to (e.g.) derive the node's id
	// private key before the wallet is fully synced for the first time.
	_, bestHeight, err := activeChainControl.chainIO.GetBestBlock()
	if err != nil {
		return err
	}

	ltndLog.Infof("Waiting for chain backend to finish sync, "+
		"start_height=%v", bestHeight)

	select {
	case <-signal.ShutdownChannel():
		return nil
	case <-activeChainControl.wallet.InitialSyncChannel():
	}

	_, bestHeight, err = activeChainControl.chainIO.GetBestBlock()
	if err != nil {
		return err
	}

	ltndLog.Infof("Chain backend is fully synced (end_height=%v)!",
		bestHeight)

	// Finally before we start the server, we'll register the "holy
	// trinity" of interface for our current "home chain" with the active
	// chainRegistry interface.
	primaryChain := cfg.registeredChains.PrimaryChain()
	cfg.registeredChains.RegisterChain(primaryChain, activeChainControl)

	// TODO(roasbeef): add rotation
	idKeyDesc, err := activeChainControl.keyRing.DeriveKey(
		keychain.KeyLocator{
			Family: keychain.KeyFamilyNodeKey,
			Index:  0,
		},
	)
	if err != nil {
		err := fmt.Errorf("error deriving node key: %v", err)
		ltndLog.Error(err)
		return err
	}

	if cfg.Tor.Active {
		srvrLog.Infof("Proxying all network traffic via Tor "+
			"(stream_isolation=%v)! NOTE: Ensure the backend node "+
			"is proxying over Tor as well", cfg.Tor.StreamIsolation)
	}

	// If the watchtower client should be active, open the client database.
	// This is done here so that Close always executes when lndMain returns.
	var towerClientDB *wtdb.ClientDB
	if cfg.WtClient.Active {
		var err error
		towerClientDB, err = wtdb.OpenClientDB(cfg.localDatabaseDir())
		if err != nil {
			err := fmt.Errorf("unable to open watchtower client "+
				"database: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer towerClientDB.Close()
	}

	// If tor is active and either v2 or v3 onion services have been specified,
	// make a tor controller and pass it into both the watchtower server and
	// the regular lnd server.
	var torController *tor.Controller
	if cfg.Tor.Active && (cfg.Tor.V2 || cfg.Tor.V3) {
		torController = tor.NewController(
			cfg.Tor.Control, cfg.Tor.TargetIPAddress, cfg.Tor.Password,
		)

		// Start the tor controller before giving it to any other subsystems.
		if err := torController.Start(); err != nil {
			err := fmt.Errorf("unable to initialize tor controller: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer func() {
			if err := torController.Stop(); err != nil {
				ltndLog.Errorf("error stopping tor controller: %v", err)
			}
		}()
	}

	var tower *watchtower.Standalone
	if cfg.Watchtower.Active {
		// Segment the watchtower directory by chain and network.
		towerDBDir := filepath.Join(
			cfg.Watchtower.TowerDir,
			cfg.registeredChains.PrimaryChain().String(),
			normalizeNetwork(activeNetParams.Name),
		)

		towerDB, err := wtdb.OpenTowerDB(towerDBDir)
		if err != nil {
			err := fmt.Errorf("unable to open watchtower "+
				"database: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer towerDB.Close()

		towerKeyDesc, err := activeChainControl.keyRing.DeriveKey(
			keychain.KeyLocator{
				Family: keychain.KeyFamilyTowerID,
				Index:  0,
			},
		)
		if err != nil {
			err := fmt.Errorf("error deriving tower key: %v", err)
			ltndLog.Error(err)
			return err
		}

		wtCfg := &watchtower.Config{
			NetParams:      activeNetParams.Params,
			BlockFetcher:   activeChainControl.chainIO,
			DB:             towerDB,
			EpochRegistrar: activeChainControl.chainNotifier,
			Net:            cfg.net,
			NewAddress: func() (stdaddr.Address, error) {
				return activeChainControl.wallet.NewAddress(
					lnwallet.WitnessPubKey, false,
				)
			},
			NodeKeyECDH: keychain.NewPubKeyECDH(
				towerKeyDesc, activeChainControl.keyRing,
			),
			PublishTx: activeChainControl.wallet.PublishTransaction,
			ChainHash: activeNetParams.GenesisHash,
		}

		// If there is a tor controller (user wants auto hidden services), then
		// store a pointer in the watchtower config.
		if torController != nil {
			wtCfg.TorController = torController
			wtCfg.WatchtowerKeyPath = cfg.Tor.WatchtowerKeyPath

			switch {
			case cfg.Tor.V2:
				wtCfg.Type = tor.V2
			case cfg.Tor.V3:
				wtCfg.Type = tor.V3
			}
		}

		wtConfig, err := cfg.Watchtower.Apply(wtCfg, lncfg.NormalizeAddresses)
		if err != nil {
			err := fmt.Errorf("unable to configure watchtower: %v",
				err)
			ltndLog.Error(err)
			return err
		}

		tower, err = watchtower.New(wtConfig)
		if err != nil {
			err := fmt.Errorf("unable to create watchtower: %v", err)
			ltndLog.Error(err)
			return err
		}
	}

	// Initialize the ChainedAcceptor.
	chainedAcceptor := chanacceptor.NewChainedAcceptor()

	// Set up the core server which will listen for incoming peer
	// connections.
	server, err := newServer(
		cfg, cfg.Listeners, localChanDB, remoteChanDB, towerClientDB,
		activeChainControl, &idKeyDesc, walletInitParams.ChansToRestore,
		chainedAcceptor, torController,
	)
	if err != nil {
		err := fmt.Errorf("unable to create server: %v", err)
		ltndLog.Error(err)
		return err
	}

	// Set up an autopilot manager from the current config. This will be
	// used to manage the underlying autopilot agent, starting and stopping
	// it at will.
	atplCfg, err := initAutoPilot(server, cfg.Autopilot, cfg)
	if err != nil {
		err := fmt.Errorf("unable to initialize autopilot: %v", err)
		ltndLog.Error(err)
		return err
	}

	atplManager, err := autopilot.NewManager(atplCfg)
	if err != nil {
		err := fmt.Errorf("unable to create autopilot manager: %v", err)
		ltndLog.Error(err)
		return err
	}
	if err := atplManager.Start(); err != nil {
		err := fmt.Errorf("unable to start autopilot manager: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer atplManager.Stop()

	// rpcListeners is a closure we'll hand to the rpc server, that will be
	// called when it needs listeners for its GPRC server.
	rpcListeners := func() ([]*ListenerWithSignal, func(), error) {
		// If we have chosen to start with a dedicated listener for the
		// rpc server, we return it directly.
		if lisCfg.RPCListener != nil {
			return []*ListenerWithSignal{lisCfg.RPCListener},
				func() {}, nil
		}

		// Otherwise we'll return the regular listeners.
		return getListeners()
	}

	// Initialize, and register our implementation of the gRPC interface
	// exported by the rpcServer.
	rpcServer, err := newRPCServer(
		cfg, server, macaroonService, cfg.SubRPCServers, serverOpts,
		restDialOpts, restProxyDest, atplManager, server.invoices,
		tower, tlsCfg, rpcListeners, chainedAcceptor,
	)
	if err != nil {
		err := fmt.Errorf("unable to create RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	if err := rpcServer.Start(); err != nil {
		err := fmt.Errorf("unable to start RPC server: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer rpcServer.Stop()

	// With all the relevant chains initialized, we can finally start the
	// server itself.
	if err := server.Start(); err != nil {
		err := fmt.Errorf("unable to start server: %v", err)
		ltndLog.Error(err)
		return err
	}
	defer server.Stop()

	// Now that the server has started, if the autopilot mode is currently
	// active, then we'll start the autopilot agent immediately. It will be
	// stopped together with the autopilot service.
	if cfg.Autopilot.Active {
		if err := atplManager.StartAgent(); err != nil {
			err := fmt.Errorf("unable to start autopilot agent: %v",
				err)
			ltndLog.Error(err)
			return err
		}
	}

	if cfg.Watchtower.Active {
		if err := tower.Start(); err != nil {
			err := fmt.Errorf("unable to start watchtower: %v", err)
			ltndLog.Error(err)
			return err
		}
		defer tower.Stop()
	}

	// Wait for shutdown signal from either a graceful server stop or from
	// the interrupt handler.
	<-shutdownChan
	return nil
}

// getTLSConfig returns a TLS configuration for the gRPC server and credentials
// and a proxy destination for the REST reverse proxy.
func getTLSConfig(cfg *Config) (*tls.Config, *credentials.TransportCredentials,
	string, error) {

	// Ensure we create TLS key and certificate if they don't exist.
	if !fileExists(cfg.TLSCertPath) && !fileExists(cfg.TLSKeyPath) {
		rpcsLog.Infof("Generating TLS certificates...")
		err := cert.GenCertPair(
			"lnd autogenerated cert", cfg.TLSCertPath,
			cfg.TLSKeyPath, cfg.TLSExtraIPs, cfg.TLSExtraDomains,
			cfg.TLSDisableAutofill, cert.DefaultAutogenValidity,
		)
		if err != nil {
			return nil, nil, "", err
		}
		rpcsLog.Infof("Done generating TLS certificates")
	}

	certData, parsedCert, err := cert.LoadCert(
		cfg.TLSCertPath, cfg.TLSKeyPath,
	)
	if err != nil {
		return nil, nil, "", err
	}

	// We check whether the certifcate we have on disk match the IPs and
	// domains specified by the config. If the extra IPs or domains have
	// changed from when the certificate was created, we will refresh the
	// certificate if auto refresh is active.
	refresh := false
	if cfg.TLSAutoRefresh {
		refresh, err = cert.IsOutdated(
			parsedCert, cfg.TLSExtraIPs,
			cfg.TLSExtraDomains, cfg.TLSDisableAutofill,
		)
		if err != nil {
			return nil, nil, "", err
		}
	}

	// If the certificate expired or it was outdated, delete it and the TLS
	// key and generate a new pair.
	if time.Now().After(parsedCert.NotAfter) || refresh {
		ltndLog.Info("TLS certificate is expired or outdated, " +
			"generating a new one")

		err := os.Remove(cfg.TLSCertPath)
		if err != nil {
			return nil, nil, "", err
		}

		err = os.Remove(cfg.TLSKeyPath)
		if err != nil {
			return nil, nil, "", err
		}

		rpcsLog.Infof("Renewing TLS certificates...")
		err = cert.GenCertPair(
			"lnd autogenerated cert", cfg.TLSCertPath,
			cfg.TLSKeyPath, cfg.TLSExtraIPs, cfg.TLSExtraDomains,
			cfg.TLSDisableAutofill, cert.DefaultAutogenValidity,
		)
		if err != nil {
			return nil, nil, "", err
		}
		rpcsLog.Infof("Done renewing TLS certificates")

		// Reload the certificate data.
		certData, _, err = cert.LoadCert(
			cfg.TLSCertPath, cfg.TLSKeyPath,
		)
		if err != nil {
			return nil, nil, "", err
		}
	}

	tlsCfg := cert.TLSConfFromCert(certData)
	restCreds, err := credentials.NewClientTLSFromFile(cfg.TLSCertPath, "")
	if err != nil {
		return nil, nil, "", err
	}

	restProxyDest := cfg.RPCListeners[0].String()
	switch {
	case strings.Contains(restProxyDest, "0.0.0.0"):
		restProxyDest = strings.Replace(
			restProxyDest, "0.0.0.0", "127.0.0.1", 1,
		)

	case strings.Contains(restProxyDest, "[::]"):
		restProxyDest = strings.Replace(
			restProxyDest, "[::]", "[::1]", 1,
		)
	}

	return tlsCfg, &restCreds, restProxyDest, nil
}

// fileExists reports whether the named file or directory exists.
// This function is taken from https://github.com/decred/dcrd
func fileExists(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}
	return true
}

// genMacaroons generates three macaroon files; one admin-level, one for
// invoice access and one read-only. These can also be used to generate more
// granular macaroons.
func genMacaroons(ctx context.Context, svc *macaroons.Service,
	admFile, roFile, invoiceFile string) error {

	// First, we'll generate a macaroon that only allows the caller to
	// access invoice related calls. This is useful for merchants and other
	// services to allow an isolated instance that can only query and
	// modify invoices.
	invoiceMac, err := svc.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, invoicePermissions...,
	)
	if err != nil {
		return err
	}
	invoiceMacBytes, err := invoiceMac.M().MarshalBinary()
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(invoiceFile, invoiceMacBytes, 0644)
	if err != nil {
		os.Remove(invoiceFile)
		return err
	}

	// Generate the read-only macaroon and write it to a file.
	roMacaroon, err := svc.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, readPermissions...,
	)
	if err != nil {
		return err
	}
	roBytes, err := roMacaroon.M().MarshalBinary()
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(roFile, roBytes, 0644); err != nil {
		os.Remove(admFile)
		return err
	}

	// Generate the admin macaroon and write it to a file.
	adminPermissions := append(readPermissions, writePermissions...)
	admMacaroon, err := svc.NewMacaroon(
		ctx, macaroons.DefaultRootKeyID, adminPermissions...,
	)
	if err != nil {
		return err
	}
	admBytes, err := admMacaroon.M().MarshalBinary()
	if err != nil {
		return err
	}
	if err = ioutil.WriteFile(admFile, admBytes, 0600); err != nil {
		return err
	}

	return nil
}

// WalletUnlockParams holds the variables used to parameterize the unlocking of
// lnd's wallet after it has already been created.
type WalletUnlockParams struct {
	// Password is the public and private wallet passphrase.
	Password []byte

	// Birthday specifies the approximate time that this wallet was created.
	// This is used to bound any rescans on startup.
	Birthday time.Time

	// RecoveryWindow specifies the address lookahead when entering recovery
	// mode. A recovery will be attempted if this value is non-zero.
	RecoveryWindow uint32

	// Wallet is the loaded and unlocked Wallet. This is returned
	// from the unlocker service to avoid it being unlocked twice (once in
	// the unlocker service to check if the password is correct and again
	// later when lnd actually uses it). Because unlocking involves scrypt
	// which is resource intensive, we want to avoid doing it twice.
	Wallet *wallet.Wallet

	// Loader is the wallet loader used to create or open the corresponding
	// wallet.
	Loader *walletloader.Loader

	// Conn is the connection to the remote wallet when that is used
	// instead of an embedded dcrwallet instance.
	Conn *grpc.ClientConn

	// ChansToRestore a set of static channel backups that should be
	// restored before the main server instance starts up.
	ChansToRestore walletunlocker.ChannelsToRecover
}

// waitForWalletPassword will spin up gRPC and REST endpoints for the
// WalletUnlocker server, and block until a password is provided by
// the user to this RPC server.
func waitForWalletPassword(cfg *Config, restEndpoints []net.Addr,
	serverOpts []grpc.ServerOption, restDialOpts []grpc.DialOption,
	restProxyDest string, tlsConf *tls.Config,
	getListeners rpcListeners, chanDB *channeldb.DB) (*WalletUnlockParams, error) {

	// Start a gRPC server listening for HTTP/2 connections, solely used
	// for getting the encryption password from the client.
	listeners, cleanup, err := getListeners()
	if err != nil {
		return nil, err
	}
	defer cleanup()

	// Set up a new PasswordService, which will listen for passwords
	// provided over RPC.
	grpcServer := grpc.NewServer(serverOpts...)
	defer func() {
		// Unfortunately the grpc lib does not offer any external
		// method to check if there are existing connections and while
		// it claims GracefulStop() will wait for outstanding RPC calls
		// to finish, some client libraries (specifically: grpc-js used
		// in nodejs/Electron apps) have trouble when the connection is
		// closed before the response to the Unlock() call is
		// completely processed. So we add a delay here to ensure
		// there's enough time before closing the grpc listener for any
		// clients to finish processing.
		time.Sleep(100 * time.Millisecond)
		grpcServer.GracefulStop()
	}()

	// The macaroon files are passed to the wallet unlocker since they are
	// also encrypted with the wallet's password. These files will be
	// deleted within it and recreated when successfully changing the
	// wallet's password.
	macaroonFiles := []string{
		filepath.Join(cfg.networkDir, macaroons.DBFilename),
		cfg.AdminMacPath, cfg.ReadMacPath, cfg.InvoiceMacPath,
	}
	pwService := walletunlocker.New(
		cfg.ChainDir, activeNetParams.Params, !cfg.SyncFreelist,
		macaroonFiles, chanDB, cfg.Dcrwallet.GRPCHost, cfg.Dcrwallet.CertPath,
		cfg.Dcrwallet.ClientKeyPath, cfg.Dcrwallet.ClientCertPath,
		cfg.Dcrwallet.AccountNumber,
	)
	lnrpc.RegisterWalletUnlockerServer(grpcServer, pwService)

	// Use a WaitGroup so we can be sure the instructions on how to input the
	// password is the last thing to be printed to the console.
	var wg sync.WaitGroup

	for _, lis := range listeners {
		wg.Add(1)
		go func(lis *ListenerWithSignal) {
			rpcsLog.Infof("password RPC server listening on %s",
				lis.Addr())

			// Close the ready chan to indicate we are listening.
			close(lis.Ready)

			wg.Done()
			grpcServer.Serve(lis)
		}(lis)
	}

	// Start a REST proxy for our gRPC server above.
	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	mux := proxy.NewServeMux()

	err = lnrpc.RegisterWalletUnlockerHandlerFromEndpoint(
		ctx, mux, restProxyDest, restDialOpts,
	)
	if err != nil {
		return nil, err
	}

	srv := &http.Server{Handler: allowCORS(mux, cfg.RestCORS)}

	for _, restEndpoint := range restEndpoints {
		lis, err := lncfg.TLSListenOnAddress(restEndpoint, tlsConf)
		if err != nil {
			ltndLog.Errorf(
				"password gRPC proxy unable to listen on %s",
				restEndpoint,
			)
			return nil, err
		}
		defer lis.Close()

		wg.Add(1)
		go func() {
			rpcsLog.Infof(
				"password gRPC proxy started at %s",
				lis.Addr(),
			)
			wg.Done()
			srv.Serve(lis)
		}()
	}

	// Wait for gRPC and REST servers to be up running.
	wg.Wait()

	// Wait for user to provide the password.
	ltndLog.Infof("Waiting for wallet encryption password. Use `dcrlncli " +
		"create` to create a wallet, `dcrlncli unlock` to unlock an " +
		"existing wallet, or `dcrlncli changepassword` to change the " +
		"password of an existing wallet and unlock it.")

	// We currently don't distinguish between getting a password to be used
	// for creation or unlocking, as a new wallet db will be created if
	// none exists when creating the chain control.
	select {

	// The wallet is being created for the first time, we'll check to see
	// if the user provided any entropy for seed creation. If so, then
	// we'll create the wallet early to load the seed.
	case initMsg := <-pwService.InitMsgs:
		password := initMsg.Passphrase
		cipherSeed := initMsg.WalletSeed
		recoveryWindow := initMsg.RecoveryWindow

		// Before we proceed, we'll check the internal version of the
		// seed. If it's greater than the current key derivation
		// version, then we'll return an error as we don't understand
		// this.
		if cipherSeed.InternalVersion != keychain.KeyDerivationVersion {
			return nil, fmt.Errorf("invalid internal seed version "+
				"%v, current version is %v",
				cipherSeed.InternalVersion,
				keychain.KeyDerivationVersion)
		}

		netDir := dcrwallet.NetworkDir(
			cfg.ChainDir, activeNetParams.Params,
		)
		loader := walletloader.NewLoader(activeNetParams.Params, netDir,
			wallet.DefaultGapLimit)

		// With the seed, we can now use the wallet loader to create
		// the wallet, then pass it back to avoid unlocking it again.
		birthday := cipherSeed.BirthdayTime()
		newWallet, err := loader.CreateNewWallet(
			context.TODO(), password, password, cipherSeed.Entropy[:],
		)

		if err != nil {
			// Don't leave the file open in case the new wallet
			// could not be created for whatever reason.
			if err := loader.UnloadWallet(); err != nil {
				ltndLog.Errorf("Could not unload new "+
					"wallet: %v", err)
			}
			return nil, err
		}

		return &WalletUnlockParams{
			Password:       password,
			Birthday:       birthday,
			RecoveryWindow: recoveryWindow,
			Wallet:         newWallet,
			Loader:         loader,
			ChansToRestore: initMsg.ChanBackups,
		}, nil

	// The wallet has already been created in the past, and is simply being
	// unlocked. So we'll just return these passphrases.
	case unlockMsg := <-pwService.UnlockMsgs:
		return &WalletUnlockParams{
			Password:       unlockMsg.Passphrase,
			RecoveryWindow: unlockMsg.RecoveryWindow,
			Wallet:         unlockMsg.Wallet,
			Loader:         unlockMsg.Loader,
			ChansToRestore: unlockMsg.ChanBackups,
			Conn:           unlockMsg.Conn,
		}, nil

	case <-signal.ShutdownChannel():
		return nil, fmt.Errorf("shutting down")
	}
}

// initializeDatabases extracts the current databases that we'll use for normal
// operation in the daemon. Two databases are returned: one remote and one
// local. However, only if the replicated database is active will the remote
// database point to a unique database. Otherwise, the local and remote DB will
// both point to the same local database. A function closure that closes all
// opened databases is also returned.
func initializeDatabases(ctx context.Context,
	cfg *Config) (*channeldb.DB, *channeldb.DB, func(), error) {

	ltndLog.Infof("Opening the main database, this might take a few " +
		"minutes...")

	if cfg.DB.Backend == lncfg.BoltBackend {
		ltndLog.Infof("Opening bbolt database, sync_freelist=%v",
			cfg.DB.Bolt.SyncFreelist)
	}

	startOpenTime := time.Now()

	databaseBackends, err := cfg.DB.GetBackends(
		ctx, cfg.localDatabaseDir(), cfg.networkName(),
	)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("unable to obtain database "+
			"backends: %v", err)
	}

	// If the remoteDB is nil, then we'll just open a local DB as normal,
	// having the remote and local pointer be the exact same instance.
	var (
		localChanDB, remoteChanDB *channeldb.DB
		closeFuncs                []func()
	)
	if databaseBackends.RemoteDB == nil {
		// Open the channeldb, which is dedicated to storing channel,
		// and network related metadata.
		localChanDB, err = channeldb.CreateWithBackend(
			databaseBackends.LocalDB,
			channeldb.OptionSetRejectCacheSize(cfg.Caches.RejectCacheSize),
			channeldb.OptionSetChannelCacheSize(cfg.Caches.ChannelCacheSize),
			channeldb.OptionDryRunMigration(cfg.DryRunMigration),
		)
		switch {
		case err == channeldb.ErrDryRunMigrationOK:
			return nil, nil, nil, err

		case err != nil:
			err := fmt.Errorf("unable to open local channeldb: %v", err)
			ltndLog.Error(err)
			return nil, nil, nil, err
		}

		closeFuncs = append(closeFuncs, func() {
			localChanDB.Close()
		})

		remoteChanDB = localChanDB
	} else {
		ltndLog.Infof("Database replication is available! Creating " +
			"local and remote channeldb instances")

		// Otherwise, we'll open two instances, one for the state we
		// only need locally, and the other for things we want to
		// ensure are replicated.
		localChanDB, err = channeldb.CreateWithBackend(
			databaseBackends.LocalDB,
			channeldb.OptionSetRejectCacheSize(cfg.Caches.RejectCacheSize),
			channeldb.OptionSetChannelCacheSize(cfg.Caches.ChannelCacheSize),
			channeldb.OptionDryRunMigration(cfg.DryRunMigration),
		)
		switch {
		// As we want to allow both versions to get thru the dry run
		// migration, we'll only exit the second time here once the
		// remote instance has had a time to migrate as well.
		case err == channeldb.ErrDryRunMigrationOK:
			ltndLog.Infof("Local DB dry run migration successful")

		case err != nil:
			err := fmt.Errorf("unable to open local channeldb: %v", err)
			ltndLog.Error(err)
			return nil, nil, nil, err
		}

		closeFuncs = append(closeFuncs, func() {
			localChanDB.Close()
		})

		ltndLog.Infof("Opening replicated database instance...")

		remoteChanDB, err = channeldb.CreateWithBackend(
			databaseBackends.RemoteDB,
			channeldb.OptionDryRunMigration(cfg.DryRunMigration),
		)
		switch {
		case err == channeldb.ErrDryRunMigrationOK:
			return nil, nil, nil, err

		case err != nil:
			localChanDB.Close()

			err := fmt.Errorf("unable to open remote channeldb: %v", err)
			ltndLog.Error(err)
			return nil, nil, nil, err
		}

		closeFuncs = append(closeFuncs, func() {
			remoteChanDB.Close()
		})
	}

	openTime := time.Since(startOpenTime)
	ltndLog.Infof("Database now open (time_to_open=%v)!", openTime)

	cleanUp := func() {
		for _, closeFunc := range closeFuncs {
			closeFunc()
		}
	}

	return localChanDB, remoteChanDB, cleanUp, nil
}
