package tests

import (
	"context"
	"fmt"
	"testing"

	"cosmossdk.io/math"
	transfertypes "github.com/cosmos/ibc-go/v6/modules/apps/transfer/types"
	test "github.com/decentrio/rollup-e2e-testing"
	"github.com/decentrio/rollup-e2e-testing/blockdb"
	"github.com/decentrio/rollup-e2e-testing/cosmos"
	"github.com/decentrio/rollup-e2e-testing/cosmos/hub/dym_hub"
	"github.com/decentrio/rollup-e2e-testing/cosmos/rollapp/dym_rollapp"
	"github.com/decentrio/rollup-e2e-testing/ibc"

	dymensiontesting "github.com/decentrio/rollup-e2e-testing/dymension"
	"github.com/decentrio/rollup-e2e-testing/relayer"
	"github.com/decentrio/rollup-e2e-testing/testreporter"
	"github.com/decentrio/rollup-e2e-testing/testutil"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// This test case verifies the system's behavior when an eIBC packet sent from the rollapp to the hub
// that is fulfilled by the market maker
func TestEIBCFulfillment(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	ctx := context.Background()

	configFileOverrides := make(map[string]any)
	dymintTomlOverrides := make(testutil.Toml)
	dymintTomlOverrides["settlement_layer"] = "dymension"
	dymintTomlOverrides["node_address"] = "http://dymension_100-1-val-0-TestEIBCFulfillment:26657"
	dymintTomlOverrides["rollapp_id"] = "demo-dymension-rollapp"

	configFileOverrides["config/dymint.toml"] = dymintTomlOverrides
	const BLOCK_FINALITY_PERIOD = 50
	modifyGenesisKV := []cosmos.GenesisKV{
		{
			Key:   "app_state.rollapp.params.dispute_period_in_blocks",
			Value: fmt.Sprint(BLOCK_FINALITY_PERIOD),
		},
	}

	// Create chain factory with dymension
	numHubVals := 1
	numHubFullNodes := 1
	numRollAppFn := 0
	numRollAppVals := 1
	cf := test.NewBuiltinChainFactory(zaptest.NewLogger(t), []*test.ChainSpec{
		{
			Name: "rollapp1",
			ChainConfig: ibc.ChainConfig{
				Type:                "rollapp-dym",
				Name:                "rollapp-temp",
				ChainID:             "demo-dymension-rollapp",
				Images:              []ibc.DockerImage{rollappImage},
				Bin:                 "rollappd",
				Bech32Prefix:        "rol",
				Denom:               "urax",
				CoinType:            "118",
				GasPrices:           "0.0urax",
				GasAdjustment:       1.1,
				TrustingPeriod:      "112h",
				NoHostMount:         false,
				ModifyGenesis:       nil,
				ConfigFileOverrides: configFileOverrides,
			},
			NumValidators: &numRollAppVals,
			NumFullNodes:  &numRollAppFn,
		},
		{
			Name: "dymension-hub",
			ChainConfig: ibc.ChainConfig{
				Type:                "hub-dym",
				Name:                "dymension",
				ChainID:             "dymension_100-1",
				Images:              []ibc.DockerImage{dymensionImage},
				Bin:                 "dymd",
				Bech32Prefix:        "dym",
				Denom:               "udym",
				CoinType:            "118",
				GasPrices:           "0.0udym",
				EncodingConfig:      evmConfig(),
				GasAdjustment:       1.1,
				TrustingPeriod:      "112h",
				NoHostMount:         false,
				ModifyGenesis:       cosmos.ModifyGenesis(modifyGenesisKV),
				ConfigFileOverrides: nil,
			},
			NumValidators: &numHubVals,
			NumFullNodes:  &numHubFullNodes,
		},
	})

	// Get chains from the chain factory
	chains, err := cf.Chains(t.Name())
	require.NoError(t, err)

	rollapp1 := chains[0].(*dym_rollapp.DymRollApp)
	dymension := chains[1].(*dym_hub.DymHub)

	// Relayer Factory
	client, network := test.DockerSetup(t)
	r := test.NewBuiltinRelayerFactory(ibc.CosmosRly, zaptest.NewLogger(t),
		relayer.CustomDockerImage("ghcr.io/cosmos/relayer", "reece-v2.3.1-ethermint", "100:1000"),
	).Build(t, client, network)
	const ibcPath = "ibc-path"
	ic := test.NewSetup().
		AddRollUp(dymension, rollapp1).
		AddRelayer(r, "relayer").
		AddLink(test.InterchainLink{
			Chain1:  dymension,
			Chain2:  rollapp1,
			Relayer: r,
			Path:    ibcPath,
		})

	rep := testreporter.NewNopReporter()
	eRep := rep.RelayerExecReporter(t)

	err = ic.Build(ctx, eRep, test.InterchainBuildOptions{
		TestName:         t.Name(),
		Client:           client,
		NetworkID:        network,
		SkipPathCreation: false,

		// This can be used to write to the block database which will index all block data e.g. txs, msgs, events, etc.
		// BlockDatabaseFile: test.DefaultBlockDatabaseFilepath(),
	})
	require.NoError(t, err)

	walletAmount := math.NewInt(1_000_000_000_000)

	// Create some user accounts on both chains
	users := test.GetAndFundTestUsers(t, ctx, t.Name(), walletAmount, dymension, dymension, rollapp1)

	// Wait a few blocks for relayer to start and for user accounts to be created
	err = testutil.WaitForBlocks(ctx, 5, dymension, rollapp1)
	require.NoError(t, err)

	// Get our Bech32 encoded user addresses
	dymensionUser, marketMaker, rollappUser := users[0], users[1], users[2]

	dymensionUserAddr := dymensionUser.FormattedAddress()
	marketMakerAddr := marketMaker.FormattedAddress()
	rollappUserAddr := rollappUser.FormattedAddress()

	// Assert the accounts were funded
	testutil.AssertBalance(t, ctx, dymension, dymensionUserAddr, dymension.Config().Denom, walletAmount)
	testutil.AssertBalance(t, ctx, dymension, marketMakerAddr, dymension.Config().Denom, walletAmount)
	testutil.AssertBalance(t, ctx, rollapp1, rollappUserAddr, rollapp1.Config().Denom, walletAmount)

	// Compose an IBC transfer and send from dymension -> rollapp
	transferAmount := math.NewInt(1_000_000)
	multiplier := math.NewInt(10)

	eibcFee := transferAmount.Quo(multiplier) // transferAmount * 0.1
	transferAmountWithoutFee := transferAmount.Sub(eibcFee)

	channel, err := ibc.GetTransferChannel(ctx, r, eRep, dymension.Config().ChainID, rollapp1.Config().ChainID)
	require.NoError(t, err)

	transferData := ibc.WalletData{
		Address: dymensionUserAddr,
		Denom:   rollapp1.Config().Denom,
		Amount:  transferAmount,
	}

	err = r.StartRelayer(ctx, eRep, ibcPath)
	require.NoError(t, err)

	var options ibc.TransferOptions
	// set eIBC specific memo
	options.Memo = BuildEIbcMemo(eibcFee)

	_, err = rollapp1.SendIBCTransfer(ctx, channel.ChannelID, rollappUserAddr, transferData, options)
	require.NoError(t, err)

	// get eIbc event
	eibcEvents, err := getEIbcEventsWithinBlockRange(ctx, dymension, 20)
	require.NoError(t, err)
	fmt.Println("Event:", eibcEvents[0])

	// fulfill demand order
	txhash, err := dymension.FullfillDemandOrder(ctx, eibcEvents[0].ID, marketMakerAddr)
	require.NoError(t, err)
	fmt.Println(txhash)

	eibcEvents, err = getEIbcEventsWithinBlockRange(ctx, dymension, 20)
	require.NoError(t, err)
	fmt.Println("After order fulfillment:", eibcEvents[0])

	// wait a few blocks and verify sender received funds on the hub
	err = testutil.WaitForBlocks(ctx, 5, dymension)
	require.NoError(t, err)
	// Get the IBC denom for urax on Hub
	rollappTokenDenom := transfertypes.GetPrefixedDenom(channel.Counterparty.PortID, channel.Counterparty.ChannelID, rollapp1.Config().Denom)
	rollappIBCDenom := transfertypes.ParseDenomTrace(rollappTokenDenom).IBCDenom()

	// verify funds minus fee were added to receiver's address
	testutil.AssertBalance(t, ctx, dymension, dymensionUserAddr, rollappIBCDenom, transferAmountWithoutFee)
	// verify funds were deducted from market maker's wallet address
	testutil.AssertBalance(t, ctx, dymension, marketMakerAddr, rollappIBCDenom, walletAmount.Sub(transferAmountWithoutFee))
	// wait until packet finalization and verify funds + fee were added to market maker's wallet address
	err = testutil.WaitForBlocks(ctx, BLOCK_FINALITY_PERIOD, dymension)
	require.NoError(t, err)
	testutil.AssertBalance(t, ctx, dymension, marketMakerAddr, rollappIBCDenom, walletAmount.Sub(transferAmountWithoutFee).Add(transferData.Amount))

	fmt.Println("Now waiting 500 blocks...")
	err = testutil.WaitForBlocks(ctx, 500, rollapp1)
	require.NoError(t, err)

	t.Cleanup(
		func() {
			err := r.StopRelayer(ctx, eRep)
			if err != nil {
				t.Logf("an error occurred while stopping the relayer: %s", err)
			}
		},
	)
}

func getEIbcEventsWithinBlockRange(
	ctx context.Context,
	dymension *dym_hub.DymHub,
	blockRange uint64,
) ([]dymensiontesting.EibcEvent, error) {
	var eibcEventsArray []dymensiontesting.EibcEvent

	height, err := dymension.Height(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get Dymension height: %w", err)
	}
	fmt.Printf("Dymension height: %d\n", height)

	err = testutil.WaitForBlocks(ctx, int(blockRange), dymension)
	if err != nil {
		return nil, fmt.Errorf("error waiting for blocks: %w", err)
	}

	eibcEvents, err := getEventsOfType(dymension.CosmosChain, height, height+blockRange, "eibc", true)
	if err != nil {
		return nil, fmt.Errorf("error getting events of type 'eibc': %w", err)
	}

	if len(eibcEvents) == 0 {
		return nil, fmt.Errorf("There wasn't a single 'eibc' event registered within the specified block range on the hub")
	}

	for _, event := range eibcEvents {
		eibcEvent, err := dymensiontesting.MapToEibcEvent(event)
		if err != nil {
			return nil, fmt.Errorf("error mapping to EibcEvent: %w", err)
		}
		eibcEventsArray = append(eibcEventsArray, eibcEvent)
	}

	return eibcEventsArray, nil
}

func getEventsOfType(chain *cosmos.CosmosChain, startHeight uint64, endHeight uint64, eventType string, breakOnFirstOccurence bool) ([]blockdb.Event, error) {
	var eventTypeArray []blockdb.Event
	shouldReturn := false

	for height := startHeight; height <= endHeight && !shouldReturn; height++ {
		txs, err := chain.FindTxs(context.Background(), height)
		if err != nil {
			return nil, fmt.Errorf("error fetching transactions at height %d: %w", height, err)
		}

		for _, tx := range txs {
			for _, event := range tx.Events {
				if event.Type == eventType {
					eventTypeArray = append(eventTypeArray, event)
					if breakOnFirstOccurence {
						shouldReturn = true
						fmt.Println("eibc found!")
						break
					}
				}
			}
			if shouldReturn {
				break
			}
		}
	}

	return eventTypeArray, nil
}

func BuildEIbcMemo(eibcFee math.Int) string {
	return fmt.Sprintf(`{"eibc": {"fee": "%s"}}`, eibcFee.String())
}