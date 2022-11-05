package avail

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"time"

	"github.com/0xPolygon/polygon-edge/blockchain"
	"github.com/0xPolygon/polygon-edge/chain"
	"github.com/0xPolygon/polygon-edge/consensus"
	"github.com/0xPolygon/polygon-edge/helper/progress"
	"github.com/0xPolygon/polygon-edge/network"
	"github.com/0xPolygon/polygon-edge/syncer"

	//"github.com/0xPolygon/polygon-edge/protocol"
	"github.com/0xPolygon/polygon-edge/crypto"
	"github.com/0xPolygon/polygon-edge/secrets"
	"github.com/0xPolygon/polygon-edge/state"
	"github.com/0xPolygon/polygon-edge/txpool"
	"github.com/0xPolygon/polygon-edge/types"
	"github.com/ethereum/go-ethereum/accounts"
	"github.com/ethereum/go-ethereum/accounts/keystore"
	"github.com/ethereum/go-ethereum/common"
	"github.com/hashicorp/go-hclog"
	"github.com/maticnetwork/avail-settlement/pkg/avail"
	"github.com/maticnetwork/avail-settlement/pkg/staking"
	"github.com/maticnetwork/avail-settlement/pkg/test"
)

const (
	// For now hand coded address of the sequencer
	SequencerAddress = "0xF817d12e6933BbA48C14D4c992719B46aD9f5f61"

	// For now hand coded address of the watch tower
	WatchTowerAddress = "0xF817d12e6933BbA48C14D4c992719B46aD9f5f61"
)

// Dev consensus protocol seals any new transaction immediately
type Avail struct {
	logger      hclog.Logger
	availClient avail.Client
	mechanisms  []MechanismType
	nodeType    MechanismType

	syncer syncer.Syncer // Reference to the sync protocol

	notifyCh chan struct{}
	closeCh  chan struct{}

	validatorKey     *ecdsa.PrivateKey // nolint:unused // Private key for the validator
	validatorKeyAddr types.Address     // nolint:unused

	interval uint64
	txpool   *txpool.TxPool

	blockchain *blockchain.Blockchain
	executor   *state.Executor
	verifier   blockchain.Verifier

	updateCh chan struct{} // nolint:unused // Update channel

	network        *network.Server // Reference to the networking layer
	secretsManager secrets.SecretsManager
	blockTime      time.Duration // Minimum block generation time in seconds
}

// Factory implements the base factory method
func Factory(
	params *consensus.Params,
) (consensus.Consensus, error) {
	logger := params.Logger.Named("avail")

	bs, err := params.SecretsManager.GetSecret(secrets.ValidatorKey)
	if err != nil {
		panic("can't find validator key! - " + err.Error())
	}

	validatorKey, err := crypto.BytesToECDSAPrivateKey(bs)
	if err != nil {
		panic("validator key decoding failed: " + err.Error())
	}

	validatorAddr := crypto.PubKeyToAddress(&validatorKey.PublicKey)

	asq := staking.NewActiveSequencersQuerier(params.Blockchain, params.Executor, logger)
	d := &Avail{
		logger:         logger,
		notifyCh:       make(chan struct{}),
		closeCh:        make(chan struct{}),
		blockchain:     params.Blockchain,
		executor:       params.Executor,
		verifier:       staking.NewVerifier(asq, logger.Named("verifier")),
		txpool:         params.TxPool,
		secretsManager: params.SecretsManager,
		network:        params.Network,
		blockTime:      time.Duration(params.BlockTime) * time.Second,
		nodeType:       MechanismType(params.NodeType),
		syncer: syncer.NewSyncer(
			params.Logger,
			params.Network,
			params.Blockchain,
			time.Duration(params.BlockTime)*3*time.Second,
		),
		validatorKey:     validatorKey,
		validatorKeyAddr: validatorAddr,
	}

	if d.mechanisms, err = ParseMechanismConfigTypes(params.Config.Config["mechanisms"]); err != nil {
		return nil, fmt.Errorf("invalid avail mechanism type/s provided")
	}

	d.availClient, err = avail.NewClient(fmt.Sprintf("ws://%s/v1/json-rpc", params.AvailAddr.String()))
	if err != nil {
		return nil, err
	}

	rawInterval, ok := params.Config.Config["interval"]
	if ok {
		interval, ok := rawInterval.(uint64)
		if !ok {
			return nil, fmt.Errorf("interval expected int")
		}

		d.interval = interval
	}

	return d, nil
}

// Initialize initializes the consensus
func (d *Avail) Initialize() error {
	return nil
}

// Start starts the consensus mechanism
// TODO: GRPC interface and listener, validator sequence and initialization as well P2P networking
func (d *Avail) Start() error {
	if d.nodeType == Sequencer {
		// Only start the syncer for sequencer. Validator and Watch Tower are
		// working purely out of Avail.
		if err := d.syncer.Start(); err != nil {
			return err
		}

		// Ensure that sequencer always has balance
		depositBalance(d.validatorKeyAddr, big.NewInt(0).Mul(big.NewInt(100), test.ETH), d.blockchain, d.executor)
		d.logger.Error("automatic sequencer balance deposit active; remove this ASAP ...^")

		sequencerQuerier := staking.NewActiveSequencersQuerier(d.blockchain, d.executor, d.logger)
		minerAddr := d.validatorKeyAddr
		minerPk := d.validatorKey

		sequencerStaked, sequencerError := sequencerQuerier.Contains(minerAddr)
		if sequencerError != nil {
			d.logger.Error("failed to check if sequencer is staked", "err", sequencerError)
			return sequencerError
		}

		if !sequencerStaked {
			stakeAmount := big.NewInt(0).Mul(big.NewInt(10), staking.ETH)
			err := staking.Stake(d.blockchain, d.executor, d.logger, "sequencer", minerAddr, minerPk, stakeAmount, 1_000_000, "sequencer")
			if err != nil {
				d.logger.Error("failure to build staking block", "error", err)
				return err
			}
		}

		go d.runSequencer(accounts.Account{Address: common.Address(minerAddr)}, &keystore.Key{PrivateKey: minerPk})
	}

	if d.nodeType == Validator {
		go d.runValidator()
	}

	if d.nodeType == WatchTower {
		_, wtAccount, wtPK, err := getAccountData(WatchTowerAddress)
		if err != nil {
			return err
		}

		go d.runWatchTower(wtAccount, wtPK)
	}

	return nil
}

// REQUIRED BASE INTERFACE METHODS //

func (d *Avail) VerifyHeader(header *types.Header) error {
	return d.verifier.VerifyHeader(header)
}

func (d *Avail) ProcessHeaders(headers []*types.Header) error {
	return d.verifier.ProcessHeaders(headers)
}

func (d *Avail) GetBlockCreator(header *types.Header) (types.Address, error) {
	return d.verifier.GetBlockCreator(header)
}

// PreCommitState a hook to be called before finalizing state transition on inserting block
func (d *Avail) PreCommitState(header *types.Header, tx *state.Transition) error {
	return d.verifier.PreCommitState(header, tx)
}

func (d *Avail) GetSyncProgression() *progress.Progression {
	return nil //d.syncer.GetSyncProgression()
}

func (d *Avail) Prepare(header *types.Header) error {
	// TODO: Remove
	return nil
}

func (d *Avail) Seal(block *types.Block, ctx context.Context) (*types.Block, error) {
	// TODO: Remove
	return nil, nil
}

func (d *Avail) Close() error {
	close(d.closeCh)

	return nil
}

func depositBalance(receiver types.Address, amount *big.Int, bchain *blockchain.Blockchain, executor *state.Executor) {
	parent := bchain.Header()
	if parent == nil {
		panic("couldn't load header for HEAD block")
	}

	header := &types.Header{
		ParentHash: parent.Hash,
		Number:     parent.Number + 1,
		Miner:      receiver.Bytes(),
		Nonce:      types.Nonce{},
		GasLimit:   parent.GasLimit,
		Timestamp:  uint64(time.Now().Unix()),
	}

	transition, err := executor.BeginTxn(parent.StateRoot, header, receiver)
	if err != nil {
		panic("failed to begin transition: " + err.Error())
	}

	err = transition.SetAccountDirectly(receiver, &chain.GenesisAccount{Balance: amount})
	if err != nil {
		panic("failed to set account balance directly: " + err.Error())
	}

	// Commit the changes
	_, root := transition.Commit()

	// Update the header
	header.StateRoot = root
	header.GasUsed = transition.TotalGas()

	// Build the actual block
	// The header hash is computed inside `BuildBlock()`
	blk := consensus.BuildBlock(consensus.BuildBlockParams{
		Header:   header,
		Txns:     []*types.Transaction{},
		Receipts: transition.Receipts(),
	})

	// Compute the hash, this is only a provisional hash since the final one
	// is sealed after all the committed seals
	blk.Header.ComputeHash()

	err = bchain.WriteBlock(blk, "test")
	if err != nil {
		panic("failed to write balance transfer block: " + err.Error())
	}
}

// TODO: This is just a demo implementation, to get miner & watch tower
// addresses working. Implementing bare minimum out of which, when working
// correctly we can extract into more proper functions in the future.
func getAccountData(address string) (*keystore.KeyStore, accounts.Account, *keystore.Key, error) {
	ks := keystore.NewKeyStore("./data/wallets", keystore.StandardScryptN, keystore.StandardScryptP)
	acc, err := ks.Find(accounts.Account{Address: common.HexToAddress(address)})
	if err != nil {
		return nil, accounts.Account{}, nil, fmt.Errorf("failure to load sequencer miner account: %s", err)
	}

	passpharse := "secret"
	keyjson, err := ks.Export(acc, passpharse, passpharse)
	if err != nil {
		return nil, accounts.Account{}, nil, err
	}

	privatekey, err := keystore.DecryptKey(keyjson, passpharse)
	if err != nil {
		return nil, accounts.Account{}, nil, err
	}

	return ks, acc, privatekey, err
}
