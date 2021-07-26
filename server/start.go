package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"

	"github.com/cosmos/cosmos-sdk/codec"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	abciserver "github.com/tendermint/tendermint/abci/server"
	tcmd "github.com/tendermint/tendermint/cmd/tendermint/commands"
	tmos "github.com/tendermint/tendermint/libs/os"
	"github.com/tendermint/tendermint/node"
	"github.com/tendermint/tendermint/p2p"
	pvm "github.com/tendermint/tendermint/privval"
	"github.com/tendermint/tendermint/proxy"
	"github.com/tendermint/tendermint/rpc/client/local"
	dbm "github.com/tendermint/tm-db"

	"github.com/cosmos/cosmos-sdk/server/rosetta"
	crgserver "github.com/cosmos/cosmos-sdk/server/rosetta/lib/server"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	"github.com/cosmos/cosmos-sdk/server"
	"github.com/cosmos/cosmos-sdk/server/api"
	serverconfig "github.com/cosmos/cosmos-sdk/server/config"
	servergrpc "github.com/cosmos/cosmos-sdk/server/grpc"
	"github.com/cosmos/cosmos-sdk/server/types"
	storetypes "github.com/cosmos/cosmos-sdk/store/types"
	sdk "github.com/cosmos/cosmos-sdk/types"

	ethlog "github.com/ethereum/go-ethereum/log"

	"github.com/tharsis/ethermint/cmd/ethermintd/config"
	ethdebug "github.com/tharsis/ethermint/ethereum/rpc/namespaces/debug"
)

// Tendermint full-node start flags
const (
	flagWithTendermint     = "with-tendermint"
	flagAddress            = "address"
	flagTransport          = "transport"
	flagTraceStore         = "trace-store"
	flagCPUProfile         = "cpu-profile"
	FlagMinGasPrices       = "minimum-gas-prices"
	FlagHaltHeight         = "halt-height"
	FlagHaltTime           = "halt-time"
	FlagInterBlockCache    = "inter-block-cache"
	FlagUnsafeSkipUpgrades = "unsafe-skip-upgrades"
	FlagTrace              = "trace"
	FlagInvCheckPeriod     = "inv-check-period"

	FlagPruning           = "pruning"
	FlagPruningKeepRecent = "pruning-keep-recent"
	FlagPruningKeepEvery  = "pruning-keep-every"
	FlagPruningInterval   = "pruning-interval"
	FlagIndexEvents       = "index-events"
	FlagMinRetainBlocks   = "min-retain-blocks"
)

// GRPC-related flags.
const (
	flagGRPCEnable          = "grpc.enable"
	flagGRPCAddress         = "grpc.address"
	flagEVMRPCEnable        = "evm-rpc.enable"
	flagEVMRPCAddress       = "evm-rpc.address"
	flagEVMWSAddress        = "evm-rpc.ws-address"
	flagEVMEnableUnsafeCORS = "evm-rpc.enable-unsafe-cors"
	flagGRPCWebEnable       = "grpc-web.enable"
	flagGRPCWebAddress      = "grpc-web.address"
)

// State sync-related flags.
const (
	FlagStateSyncSnapshotInterval   = "state-sync.snapshot-interval"
	FlagStateSyncSnapshotKeepRecent = "state-sync.snapshot-keep-recent"
)

// StartCmd runs the service passed in, either stand-alone or in-process with
// Tendermint.
func StartCmd(appCreator types.AppCreator, defaultNodeHome string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Run the full node",
		Long: `Run the full node application with Tendermint in or out of process. By
default, the application will run with Tendermint in process.

Pruning options can be provided via the '--pruning' flag or alternatively with '--pruning-keep-recent',
'pruning-keep-every', and 'pruning-interval' together.

For '--pruning' the options are as follows:

default: the last 100 states are kept in addition to every 500th state; pruning at 10 block intervals
nothing: all historic states will be saved, nothing will be deleted (i.e. archiving node)
everything: all saved states will be deleted, storing only the current state; pruning at 10 block intervals
custom: allow pruning options to be manually specified through 'pruning-keep-recent', 'pruning-keep-every', and 'pruning-interval'

Node halting configurations exist in the form of two flags: '--halt-height' and '--halt-time'. During
the ABCI Commit phase, the node will check if the current block height is greater than or equal to
the halt-height or if the current block time is greater than or equal to the halt-time. If so, the
node will attempt to gracefully shutdown and the block will not be committed. In addition, the node
will not be able to commit subsequent blocks.

For profiling and benchmarking purposes, CPU profiling can be enabled via the '--cpu-profile' flag
which accepts a path for the resulting pprof file.
`,
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			serverCtx := server.GetServerContextFromCmd(cmd)

			// Bind flags to the Context's Viper so the app construction can set
			// options accordingly.
			err := serverCtx.Viper.BindPFlags(cmd.Flags())
			if err != nil {
				return err
			}

			_, err = server.GetPruningOptionsFromFlags(serverCtx.Viper)
			return err
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			serverCtx := server.GetServerContextFromCmd(cmd)
			clientCtx, err := client.GetClientQueryContext(cmd)
			if err != nil {
				return err
			}

			withTM, _ := cmd.Flags().GetBool(flagWithTendermint)
			if !withTM {
				serverCtx.Logger.Info("starting ABCI without Tendermint")
				return startStandAlone(serverCtx, appCreator)
			}

			serverCtx.Logger.Info("starting ABCI with Tendermint")

			// amino is needed here for backwards compatibility of REST routes
			err = startInProcess(serverCtx, clientCtx, appCreator)
			errCode, ok := err.(server.ErrorCode)
			if !ok {
				return err
			}

			serverCtx.Logger.Debug(fmt.Sprintf("received quit signal: %d", errCode.Code))
			return nil
		},
	}

	cmd.Flags().String(flags.FlagHome, defaultNodeHome, "The application home directory")
	cmd.Flags().Bool(flagWithTendermint, true, "Run abci app embedded in-process with tendermint")
	cmd.Flags().String(flagAddress, "tcp://0.0.0.0:26658", "Listen address")
	cmd.Flags().String(flagTransport, "socket", "Transport protocol: socket, grpc")
	cmd.Flags().String(flagTraceStore, "", "Enable KVStore tracing to an output file")
	cmd.Flags().String(FlagMinGasPrices, "", "Minimum gas prices to accept for transactions; Any fee in a tx must meet this minimum (e.g. 0.01photino;0.0001stake)")
	cmd.Flags().IntSlice(FlagUnsafeSkipUpgrades, []int{}, "Skip a set of upgrade heights to continue the old binary")
	cmd.Flags().Uint64(FlagHaltHeight, 0, "Block height at which to gracefully halt the chain and shutdown the node")
	cmd.Flags().Uint64(FlagHaltTime, 0, "Minimum block time (in Unix seconds) at which to gracefully halt the chain and shutdown the node")
	cmd.Flags().Bool(FlagInterBlockCache, true, "Enable inter-block caching")
	cmd.Flags().String(flagCPUProfile, "", "Enable CPU profiling and write to the provided file")
	cmd.Flags().Bool(FlagTrace, false, "Provide full stack traces for errors in ABCI Log")
	cmd.Flags().String(FlagPruning, storetypes.PruningOptionDefault, "Pruning strategy (default|nothing|everything|custom)")
	cmd.Flags().Uint64(FlagPruningKeepRecent, 0, "Number of recent heights to keep on disk (ignored if pruning is not 'custom')")
	cmd.Flags().Uint64(FlagPruningKeepEvery, 0, "Offset heights to keep on disk after 'keep-every' (ignored if pruning is not 'custom')")
	cmd.Flags().Uint64(FlagPruningInterval, 0, "Height interval at which pruned heights are removed from disk (ignored if pruning is not 'custom')")
	cmd.Flags().Uint(FlagInvCheckPeriod, 0, "Assert registered invariants every N blocks")
	cmd.Flags().Uint64(FlagMinRetainBlocks, 0, "Minimum block height offset during ABCI commit to prune Tendermint blocks")

	cmd.Flags().Bool(flagGRPCEnable, true, "Define if the gRPC server should be enabled")
	cmd.Flags().String(flagGRPCAddress, serverconfig.DefaultGRPCAddress, "the gRPC server address to listen on")

	cmd.Flags().Bool(flagGRPCWebEnable, true, "Define if the gRPC-Web server should be enabled. (Note: gRPC must also be enabled.)")
	cmd.Flags().String(flagGRPCWebAddress, serverconfig.DefaultGRPCWebAddress, "The gRPC-Web server address to listen on")

	cmd.Flags().Bool(flagEVMRPCEnable, true, "Define if the gRPC server should be enabled")
	cmd.Flags().String(flagEVMRPCAddress, config.DefaultEVMAddress, "the EVM RPC server address to listen on")
	cmd.Flags().String(flagEVMWSAddress, config.DefaultEVMWSAddress, "the EVM WS server address to listen on")
	cmd.Flags().Bool(flagEVMEnableUnsafeCORS, false, "Define if the EVM RPC server should enabled CORS (unsafe - use it at your own risk)")

	cmd.Flags().Uint64(FlagStateSyncSnapshotInterval, 0, "State sync snapshot interval")
	cmd.Flags().Uint32(FlagStateSyncSnapshotKeepRecent, 2, "State sync snapshot to keep")

	// add support for all Tendermint-specific command line options
	tcmd.AddNodeFlags(cmd)
	return cmd
}

func startStandAlone(ctx *server.Context, appCreator types.AppCreator) error {
	addr := ctx.Viper.GetString(flagAddress)
	transport := ctx.Viper.GetString(flagTransport)
	home := ctx.Viper.GetString(flags.FlagHome)

	db, err := openDB(home)
	if err != nil {
		return err
	}

	traceWriterFile := ctx.Viper.GetString(flagTraceStore)
	traceWriter, err := openTraceWriter(traceWriterFile)
	if err != nil {
		return err
	}

	app := appCreator(ctx.Logger, db, traceWriter, ctx.Viper)

	svr, err := abciserver.NewServer(addr, transport, app)
	if err != nil {
		return fmt.Errorf("error creating listener: %v", err)
	}

	svr.SetLogger(ctx.Logger.With("server", "abci"))

	err = svr.Start()
	if err != nil {
		tmos.Exit(err.Error())
	}

	defer func() {
		if err = svr.Stop(); err != nil {
			tmos.Exit(err.Error())
		}
	}()

	// Wait for SIGINT or SIGTERM signal
	return server.WaitForQuitSignals()
}

// legacyAminoCdc is used for the legacy REST API
func startInProcess(ctx *server.Context, clientCtx client.Context, appCreator types.AppCreator) error {
	cfg := ctx.Config
	home := cfg.RootDir
	logger := ctx.Logger
	var cpuProfileCleanup func()

	if cpuProfile := ctx.Viper.GetString(flagCPUProfile); cpuProfile != "" {
		f, err := os.Create(ethdebug.ExpandHome(cpuProfile))
		if err != nil {
			return err
		}

		ctx.Logger.Info("starting CPU profiler", "profile", cpuProfile)
		if err := pprof.StartCPUProfile(f); err != nil {
			return err
		}

		cpuProfileCleanup = func() {
			ctx.Logger.Info("stopping CPU profiler", "profile", cpuProfile)
			pprof.StopCPUProfile()
			f.Close()
		}
	}

	traceWriterFile := ctx.Viper.GetString(flagTraceStore)
	db, err := openDB(home)
	if err != nil {
		logger.Error("failed to open DB", "error", err.Error())
		return err
	}

	traceWriter, err := openTraceWriter(traceWriterFile)
	if err != nil {
		logger.Error("failed to open trace writer", "error", err.Error())
		return err
	}

	config := config.GetConfig(ctx.Viper)
	if err := config.ValidateBasic(); err != nil {
		ctx.Logger.Error("WARNING: The minimum-gas-prices config in app.toml is set to the empty string. " +
			"This defaults to 0 in the current version, but will error in the next version " +
			"(SDK v0.44). Please explicitly put the desired minimum-gas-prices in your app.toml.")
	}

	app := appCreator(ctx.Logger, db, traceWriter, ctx.Viper)

	nodeKey, err := p2p.LoadOrGenNodeKey(cfg.NodeKeyFile())
	if err != nil {
		logger.Error("failed load or gen node key", "error", err.Error())
		return err
	}

	genDocProvider := node.DefaultGenesisDocProviderFunc(cfg)
	tmNode, err := node.NewNode(
		cfg,
		pvm.LoadOrGenFilePV(cfg.PrivValidatorKeyFile(), cfg.PrivValidatorStateFile()),
		nodeKey,
		proxy.NewLocalClientCreator(app),
		genDocProvider,
		node.DefaultDBProvider,
		node.DefaultMetricsProvider(cfg.Instrumentation),
		ctx.Logger.With("server", "node"),
	)
	if err != nil {
		logger.Error("failed init node", "error", err.Error())
		return err
	}

	if err := tmNode.Start(); err != nil {
		logger.Error("failed start tendermint server", "error", err.Error())
		return err
	}

	// Add the tx service to the gRPC router. We only need to register this
	// service if API or gRPC is enabled, and avoid doing so in the general
	// case, because it spawns a new local tendermint RPC client.
	if config.API.Enable || config.GRPC.Enable {
		clientCtx = clientCtx.WithClient(local.New(tmNode))

		app.RegisterTxService(clientCtx)
		app.RegisterTendermintService(clientCtx)
	}

	var apiSrv *api.Server
	if config.API.Enable {
		genDoc, err := genDocProvider()
		if err != nil {
			return err
		}

		clientCtx := clientCtx.
			WithHomeDir(home).
			WithChainID(genDoc.ChainID)

		apiSrv = api.New(clientCtx, ctx.Logger.With("server", "api"))
		app.RegisterAPIRoutes(apiSrv, config.API)
		errCh := make(chan error)

		go func() {
			if err := apiSrv.Start(config.Config); err != nil {
				errCh <- err
			}
		}()

		select {
		case err := <-errCh:
			return err
		case <-time.After(types.ServerStartTime): // assume server started successfully
		}
	}

	var (
		grpcSrv    *grpc.Server
		grpcWebSrv *http.Server
	)
	if config.GRPC.Enable {
		grpcSrv, err = servergrpc.StartGRPCServer(clientCtx, app, config.GRPC.Address)
		if err != nil {
			return err
		}
		if config.GRPCWeb.Enable {
			grpcWebSrv, err = servergrpc.StartGRPCWeb(grpcSrv, config.Config)
			if err != nil {
				ctx.Logger.Error("failed to start grpc-web http server: ", err)
				return err
			}
		}
	}

	var rosettaSrv crgserver.Server
	if config.Rosetta.Enable {
		offlineMode := config.Rosetta.Offline
		if !config.GRPC.Enable { // If GRPC is not enabled rosetta cannot work in online mode, so it works in offline mode.
			offlineMode = true
		}

		conf := &rosetta.Config{
			Blockchain:    config.Rosetta.Blockchain,
			Network:       config.Rosetta.Network,
			TendermintRPC: ctx.Config.RPC.ListenAddress,
			GRPCEndpoint:  config.GRPC.Address,
			Addr:          config.Rosetta.Address,
			Retries:       config.Rosetta.Retries,
			Offline:       offlineMode,
		}
		conf.WithCodec(clientCtx.InterfaceRegistry, clientCtx.Codec.(*codec.ProtoCodec))

		rosettaSrv, err = rosetta.ServerFromConfig(conf)
		if err != nil {
			return err
		}
		errCh := make(chan error)
		go func() {
			if err := rosettaSrv.Start(); err != nil {
				errCh <- err
			}
		}()

		select {
		case err := <-errCh:
			return err
		case <-time.After(types.ServerStartTime): // assume server started successfully
		}
	}

	ethlog.Root().SetHandler(ethlog.StdoutHandler)

	var (
		httpSrv     *http.Server
		httpSrvDone chan struct{}
	)
	if config.EVMRPC.Enable {
		genDoc, err := genDocProvider()
		if err != nil {
			return err
		}

		clientCtx := clientCtx.WithChainID(genDoc.ChainID)

		tmEndpoint := "/websocket"
		tmRPCAddr := cfg.RPC.ListenAddress
		httpSrv, httpSrvDone, err = StartEVMRPC(ctx, clientCtx, tmRPCAddr, tmEndpoint, config)
		if err != nil {
			return err
		}
	}

	defer func() {
		if tmNode.IsRunning() {
			_ = tmNode.Stop()
		}

		if cpuProfileCleanup != nil {
			cpuProfileCleanup()
		}

		if apiSrv != nil {
			_ = apiSrv.Close()
		}

		if grpcSrv != nil {
			grpcSrv.Stop()
			if grpcWebSrv != nil {
				grpcWebSrv.Close()
			}
		}

		if httpSrv != nil {
			shutdownCtx, cancelFn := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancelFn()

			if err := httpSrv.Shutdown(shutdownCtx); err != nil {
				logger.Error("HTTP server shutdown produced a warning", "error", err.Error())
			} else {
				logger.Info("HTTP server shut down, waiting 5 sec")
				select {
				case <-time.Tick(5 * time.Second):
				case <-httpSrvDone:
				}
			}
		}

		logger.Info("Bye!")
	}()

	// Wait for SIGINT or SIGTERM signal
	return server.WaitForQuitSignals()
}

func openDB(rootDir string) (dbm.DB, error) {
	dataDir := filepath.Join(rootDir, "data")
	return sdk.NewLevelDB("application", dataDir)
}

func openTraceWriter(traceWriterFile string) (w io.Writer, err error) {
	if traceWriterFile == "" {
		return
	}
	return os.OpenFile(
		traceWriterFile,
		os.O_WRONLY|os.O_APPEND|os.O_CREATE,
		0666,
	)
}