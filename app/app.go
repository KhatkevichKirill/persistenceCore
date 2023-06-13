/*
 Copyright [2019] - [2021], PERSISTENCE TECHNOLOGIES PTE. LTD. and the persistenceCore contributors
 SPDX-License-Identifier: Apache-2.0
*/

package app

import (
	"encoding/json"
	"fmt"
	"io"
	stdlog "log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	autocliv1 "cosmossdk.io/api/cosmos/autocli/v1"
	reflectionv1 "cosmossdk.io/api/cosmos/reflection/v1"
	"github.com/spf13/cast"

	"github.com/CosmWasm/wasmd/x/wasm"
	wasmkeeper "github.com/CosmWasm/wasmd/x/wasm/keeper"
	wasmtypes "github.com/CosmWasm/wasmd/x/wasm/types"
	tendermintdb "github.com/cometbft/cometbft-db"
	abcitypes "github.com/cometbft/cometbft/abci/types"
	tendermintjson "github.com/cometbft/cometbft/libs/json"
	tendermintlog "github.com/cometbft/cometbft/libs/log"
	tendermintos "github.com/cometbft/cometbft/libs/os"
	tendermintproto "github.com/cometbft/cometbft/proto/tendermint/types"
	"github.com/cosmos/cosmos-sdk/baseapp"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/flags"
	nodeservice "github.com/cosmos/cosmos-sdk/client/grpc/node"
	"github.com/cosmos/cosmos-sdk/client/grpc/tmservice"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/runtime"
	runtimeservices "github.com/cosmos/cosmos-sdk/runtime/services"
	"github.com/cosmos/cosmos-sdk/server/api"
	"github.com/cosmos/cosmos-sdk/server/config"
	servertypes "github.com/cosmos/cosmos-sdk/server/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/module"
	"github.com/cosmos/cosmos-sdk/version"
	"github.com/cosmos/cosmos-sdk/x/auth/ante"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	"github.com/cosmos/cosmos-sdk/x/crisis"
	paramstypes "github.com/cosmos/cosmos-sdk/x/params/types"
	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"
	"github.com/gorilla/mux"
	"github.com/rakyll/statik/fs"

	"github.com/persistenceOne/persistenceCore/v8/app/keepers"
	"github.com/persistenceOne/persistenceCore/v8/app/upgrades"
	v8 "github.com/persistenceOne/persistenceCore/v8/app/upgrades/v8"
)

var (
	DefaultNodeHome string
	Upgrades        = []upgrades.Upgrade{v8.Upgrade}
	ModuleBasics    = module.NewBasicManager(keepers.AppModuleBasics...)
)

var (
	// ProposalsEnabled is "true" and EnabledSpecificProposals is "", then enable all x/wasm proposals.
	// ProposalsEnabled is not "true" and EnabledSpecificProposals is "", then disable all x/wasm proposals.
	ProposalsEnabled = "true"
	// EnableSpecificProposals if set to non-empty string it must be comma-separated list of values that are all a subset
	// of "EnableAllProposals" (takes precedence over ProposalsEnabled)
	// https://github.com/CosmWasm/wasmd/blob/02a54d33ff2c064f3539ae12d75d027d9c665f05/x/wasm/internal/types/proposal.go#L28-L34
	EnableSpecificProposals = ""
)

// GetEnabledProposals parses the ProposalsEnabled / EnableSpecificProposals values to
// produce a list of enabled proposals to pass into wasmd app.
func GetEnabledProposals() []wasm.ProposalType {
	if EnableSpecificProposals == "" {
		if ProposalsEnabled == "true" {
			return wasm.EnableAllProposals
		}
		return wasm.DisableAllProposals
	}
	chunks := strings.Split(EnableSpecificProposals, ",")
	proposals, err := wasm.ConvertToProposals(chunks)
	if err != nil {
		panic(err)
	}
	return proposals
}

var (
	_ runtime.AppI            = (*Application)(nil)
	_ servertypes.Application = (*Application)(nil)
)

func init() {
	userHomeDir, err := os.UserHomeDir()
	if err != nil {
		stdlog.Println("Failed to get home dir %2", err)
	}

	DefaultNodeHome = filepath.Join(userHomeDir, ".persistenceCore")
}

type Application struct {
	*baseapp.BaseApp
	*keepers.AppKeepers

	legacyAmino       *codec.LegacyAmino
	applicationCodec  codec.Codec
	interfaceRegistry types.InterfaceRegistry

	moduleManager     *module.Manager
	configurator      module.Configurator
	simulationManager *module.SimulationManager
}

func NewApplication(
	logger tendermintlog.Logger,
	db tendermintdb.DB,
	traceStore io.Writer,
	loadLatest bool,
	enabledProposals []wasm.ProposalType,
	applicationOptions servertypes.AppOptions,
	wasmOpts []wasm.Option,
	baseAppOptions ...func(*baseapp.BaseApp),
) *Application {
	encodingConfiguration := MakeEncodingConfig()

	applicationCodec := encodingConfiguration.Marshaler
	legacyAmino := encodingConfiguration.Amino
	interfaceRegistry := encodingConfiguration.InterfaceRegistry
	txConfig := encodingConfiguration.TransactionConfig

	baseApp := baseapp.NewBaseApp(
		AppName,
		logger,
		db,
		txConfig.TxDecoder(),
		baseAppOptions...,
	)
	baseApp.SetCommitMultiStoreTracer(traceStore)
	baseApp.SetVersion(version.Version)
	baseApp.SetInterfaceRegistry(interfaceRegistry)
	baseApp.SetTxEncoder(txConfig.TxEncoder())

	homePath := cast.ToString(applicationOptions.Get(flags.FlagHome))
	wasmDir := filepath.Join(homePath, "wasm")
	wasmConfig, err := wasm.ReadWasmConfig(applicationOptions)
	if err != nil {
		panic(fmt.Sprintf("error while reading wasm config: %s", err))
	}

	app := &Application{
		BaseApp:           baseApp,
		legacyAmino:       legacyAmino,
		applicationCodec:  applicationCodec,
		interfaceRegistry: interfaceRegistry,
	}

	// Setup keepers
	app.AppKeepers = keepers.NewAppKeeper(
		applicationCodec,
		baseApp,
		legacyAmino,
		ModuleAccountPermissions,
		SendCoinBlockedAddrs(),
		applicationOptions,
		wasmDir,
		enabledProposals,
		wasmOpts,
		Bech32MainPrefix,
	)

	// NOTE: we may consider parsing `appOpts` inside module constructors. For the moment
	// we prefer to be more strict in what arguments the modules expect.
	skipGenesisInvariants := cast.ToBool(applicationOptions.Get(crisis.FlagSkipGenesisInvariants))

	app.moduleManager = module.NewManager(appModules(app, encodingConfiguration, skipGenesisInvariants)...)

	app.moduleManager.SetOrderBeginBlockers(orderBeginBlockers()...)
	app.moduleManager.SetOrderEndBlockers(orderEndBlockers()...)
	app.moduleManager.SetOrderInitGenesis(orderInitGenesis()...)
	app.moduleManager.SetOrderExportGenesis(orderInitGenesis()...)

	app.moduleManager.RegisterInvariants(app.CrisisKeeper)
	app.configurator = module.NewConfigurator(app.applicationCodec, app.MsgServiceRouter(), app.GRPCQueryRouter())
	app.moduleManager.RegisterServices(app.configurator)

	app.simulationManager = module.NewSimulationManagerFromAppModules(
		app.moduleManager.Modules,
		overrideSimulationModules(app, encodingConfiguration, skipGenesisInvariants),
	)
	app.simulationManager.RegisterStoreDecoders()

	app.registerGRPCServices()

	app.MountKVStores(app.GetKVStoreKey())
	app.MountTransientStores(app.GetTransientStoreKey())
	app.MountMemoryStores(app.GetMemoryStoreKey())

	app.setAnteHandler(encodingConfiguration.TransactionConfig, wasmConfig)
	app.SetInitChainer(app.InitChainer)
	app.SetBeginBlocker(app.BeginBlocker)
	app.SetEndBlocker(app.EndBlocker)

	// setup postHandler in this method
	// app.setupPostHandler()
	app.setupUpgradeHandlers()
	app.setupUpgradeStoreLoaders()

	// must be before Loading version
	// requires the snapshot store to be created and registered as a BaseAppOption
	// see cmd/wasmd/root.go: 206 - 214 approx
	if manager := app.SnapshotManager(); manager != nil {
		err := manager.RegisterExtensions(
			wasmkeeper.NewWasmSnapshotter(app.CommitMultiStore(), app.WasmKeeper),
		)
		if err != nil {
			panic(fmt.Errorf("failed to register snapshot extension: %s", err))
		}
	}

	if loadLatest {
		if err := app.BaseApp.LoadLatestVersion(); err != nil {
			tendermintos.Exit(err.Error())
		}
		ctx := app.BaseApp.NewUncachedContext(true, tendermintproto.Header{})

		// Initialize pinned codes in wasmvm as they are not persisted there
		if err := app.WasmKeeper.InitializePinnedCodes(ctx); err != nil {
			tendermintos.Exit(fmt.Sprintf("failed initialize pinned codes %s", err))
		}
	}

	return app
}

func (app *Application) setAnteHandler(txConfig client.TxConfig, wasmConfig wasmtypes.WasmConfig) {
	anteHandler, err := NewAnteHandler(
		HandlerOptions{
			HandlerOptions: ante.HandlerOptions{
				AccountKeeper:   app.AccountKeeper,
				BankKeeper:      app.BankKeeper,
				FeegrantKeeper:  app.FeegrantKeeper,
				SignModeHandler: txConfig.SignModeHandler(),
				SigGasConsumer:  ante.DefaultSigVerificationGasConsumer,
			},
			IBCKeeper:         app.IBCKeeper,
			WasmConfig:        &wasmConfig,
			TXCounterStoreKey: app.GetKVStoreKey()[wasm.StoreKey],
		},
	)
	if err != nil {
		panic(fmt.Errorf("failed to create AnteHandler: %s", err))
	}
	app.SetAnteHandler(anteHandler)
}

func (app *Application) registerGRPCServices() {
	autocliv1.RegisterQueryServer(app.GRPCQueryRouter(), runtimeservices.NewAutoCLIQueryService(app.moduleManager.Modules))

	reflectionSvc, err := runtimeservices.NewReflectionService()
	if err != nil {
		panic(err)
	}
	reflectionv1.RegisterReflectionServiceServer(app.GRPCQueryRouter(), reflectionSvc)
}

func (app *Application) ApplicationCodec() codec.Codec {
	return app.applicationCodec
}

func (app *Application) Name() string {
	return app.BaseApp.Name()
}

func (app *Application) LegacyAmino() *codec.LegacyAmino {
	return app.legacyAmino
}

func (app *Application) BeginBlocker(ctx sdk.Context, req abcitypes.RequestBeginBlock) abcitypes.ResponseBeginBlock {
	return app.moduleManager.BeginBlock(ctx, req)
}

func (app *Application) EndBlocker(ctx sdk.Context, req abcitypes.RequestEndBlock) abcitypes.ResponseEndBlock {
	// FIXME(max): remove this block after state migration is final
	if ctx.BlockHeight() > 11060956 {
		validators := app.StakingKeeper.GetLastValidators(ctx)
		for _, val := range validators {
			var valNeedsUpdate bool

			if val.TotalLiquidShares.IsNil() {
				val.TotalLiquidShares = sdk.ZeroDec()
				valNeedsUpdate = true
			}
			if val.TotalValidatorBondShares.IsNil() {
				val.TotalValidatorBondShares = sdk.ZeroDec()
				valNeedsUpdate = true
			}

			if valNeedsUpdate {
				app.StakingKeeper.SetValidator(ctx, val)
				app.Logger().Info("migrated validator fields for liquid shares", "val", val.OperatorAddress)
			}
		}
	}

	return app.moduleManager.EndBlock(ctx, req)
}

func (app *Application) InitChainer(ctx sdk.Context, req abcitypes.RequestInitChain) abcitypes.ResponseInitChain {
	var genesisState GenesisState
	if err := tendermintjson.Unmarshal(req.AppStateBytes, &genesisState); err != nil {
		panic(err)
	}

	app.UpgradeKeeper.SetModuleVersionMap(ctx, app.moduleManager.GetVersionMap())

	return app.moduleManager.InitGenesis(ctx, app.applicationCodec, genesisState)
}

func (app *Application) ModuleAccountAddrs() map[string]bool {
	modAccAddrs := make(map[string]bool)
	for acc := range ModuleAccountPermissions {
		modAccAddrs[authtypes.NewModuleAddress(acc).String()] = true
	}

	return modAccAddrs
}

func (app *Application) GetSubspace(moduleName string) paramstypes.Subspace {
	subspace, _ := app.ParamsKeeper.GetSubspace(moduleName)
	return subspace
}

func (app *Application) SimulationManager() *module.SimulationManager {
	return app.simulationManager
}

func (app *Application) RegisterAPIRoutes(apiServer *api.Server, apiConfig config.APIConfig) {
	clientCtx := apiServer.ClientCtx
	// Register new tx routes from grpc-gateway.
	authtx.RegisterGRPCGatewayRoutes(clientCtx, apiServer.GRPCGatewayRouter)
	// Register new tendermint queries routes from grpc-gateway.
	tmservice.RegisterGRPCGatewayRoutes(clientCtx, apiServer.GRPCGatewayRouter)
	// Register node gRPC service for grpc-gateway.
	nodeservice.RegisterGRPCGatewayRoutes(clientCtx, apiServer.GRPCGatewayRouter)
	// Register grpc-gateway routes for all modules.
	ModuleBasics.RegisterGRPCGatewayRoutes(clientCtx, apiServer.GRPCGatewayRouter)

	// register swagger API from root so that other applications can override easily
	if apiConfig.Swagger {
		RegisterSwaggerAPI(apiServer.Router)
	}
}

func (app *Application) setupUpgradeHandlers() {
	for _, upgrade := range Upgrades {
		app.UpgradeKeeper.SetUpgradeHandler(
			upgrade.UpgradeName,
			upgrade.CreateUpgradeHandler(upgrades.UpgradeHandlerArgs{
				ModuleManager: app.moduleManager,
				Configurator:  app.configurator,
				Keepers:       app.AppKeepers,
				Codec:         app.applicationCodec,
			}),
		)
	}
}

// configure store loader that checks if version == upgradeHeight and applies store upgrades
func (app *Application) setupUpgradeStoreLoaders() {
	upgradeInfo, err := app.UpgradeKeeper.ReadUpgradeInfoFromDisk()
	if err != nil {
		panic(fmt.Sprintf("failed to read upgrade info from disk %s", err))
	}

	if app.UpgradeKeeper.IsSkipHeight(upgradeInfo.Height) {
		return
	}

	for _, upgrade := range Upgrades {
		if upgradeInfo.Name == upgrade.UpgradeName {
			app.SetStoreLoader(upgradetypes.UpgradeStoreLoader(upgradeInfo.Height, &upgrade.StoreUpgrades))
		}
	}
}

// PostHandlers are like AnteHandlers (they have the same signature), but they are run after runMsgs.
// One use case for PostHandlers is transaction tips,
// but other use cases like unused gas refund can also be enabled by PostHandlers.
//
// In baseapp, postHandlers are run in the same store branch as `runMsgs`,
// meaning that both `runMsgs` and `postHandler` state will be committed if
// both are successful, and both will be reverted if any of the two fails.
// nolint:unused // post handle is not used for now (as there is no requirement of it)
func (app *Application) setupPostHandler() {
	postDecorators := []sdk.PostDecorator{
		// posthandler.NewTipDecorator(app.BankKeeper),
		// ... add more decorators as needed
	}
	postHandler := sdk.ChainPostDecorators(postDecorators...)
	app.SetPostHandler(postHandler)
}

func RegisterSwaggerAPI(rtr *mux.Router) {
	statikFS, err := fs.New()
	if err != nil {
		panic(err)
	}

	staticServer := http.FileServer(statikFS)
	rtr.PathPrefix("/swagger/").Handler(http.StripPrefix("/swagger/", staticServer))
}

func (app *Application) RegisterTxService(clientContect client.Context) {
	authtx.RegisterTxService(app.BaseApp.GRPCQueryRouter(), clientContect, app.BaseApp.Simulate, app.interfaceRegistry)
}

func (app *Application) RegisterTendermintService(clientCtx client.Context) {
	tmservice.RegisterTendermintService(clientCtx, app.BaseApp.GRPCQueryRouter(), app.interfaceRegistry, app.Query)
}
func (app *Application) RegisterNodeService(clientCtx client.Context) {
	nodeservice.RegisterNodeService(clientCtx, app.GRPCQueryRouter())
}
func (app *Application) LoadHeight(height int64) error {
	return app.BaseApp.LoadVersion(height)
}

// DefaultGenesis returns a default genesis from the registered AppModuleBasic's.
func (app *Application) DefaultGenesis() map[string]json.RawMessage {
	return ModuleBasics.DefaultGenesis(app.applicationCodec)
}

func SendCoinBlockedAddrs() map[string]bool {
	sendCoinBlockedAddrs := make(map[string]bool)
	for acc := range ModuleAccountPermissions {
		sendCoinBlockedAddrs[authtypes.NewModuleAddress(acc).String()] = !receiveAllowedMAcc[acc]
	}
	return sendCoinBlockedAddrs
}
