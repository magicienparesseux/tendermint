package main

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbm "github.com/tendermint/tm-db"

	abci "github.com/tendermint/tendermint/abci/types"
	bcR "github.com/tendermint/tendermint/blockchain/v1"
	cfg "github.com/tendermint/tendermint/config"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/mock"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/proxy"
	sm "github.com/tendermint/tendermint/state"
	store "github.com/tendermint/tendermint/store"
	types "github.com/tendermint/tendermint/types"
	tmtime "github.com/tendermint/tendermint/types/time"
)

var config *cfg.Config

type SApp struct {
	abci.BaseApplication
}

type SConsensusReactor struct {
	p2p.BaseReactor     // BaseService + p2p.Switch
	switchedToConsensus bool
	mtx                 sync.Mutex
}

type SBlockchainReactors struct {
	bcR  *bcR.BlockchainReactor
	conR *SConsensusReactor
}

type SBlockState struct {
	store    *store.BlockStore
	state    *sm.State
	executor *sm.BlockExecutor
}

func (conR *SConsensusReactor) SwitchToConsensus(state sm.State, blocksSynced uint64) {
	conR.mtx.Lock()
	defer conR.mtx.Unlock()
	conR.switchedToConsensus = true
}

func Txs(height int64) (txs []types.Tx) {
	for i := 0; i < 10; i++ {
		txs = append(txs, types.Tx([]byte{byte(height), byte(i)}))
	}
	return txs
}

func Block(height int64, state sm.State, lastCommit *types.Commit) *types.Block {
	block, _ := state.MakeBlock(height, makeTxs(height), lastCommit, nil, state.Validators.GetProposer().Address)
	return block
}

func RandGenesisDoc(numValidators int, randPower bool, minPower int64) (*types.GenesisDoc, []types.PrivValidator) {
	validators := make([]types.GenesisValidator, numValidators)
	privValidators := make([]types.PrivValidator, numValidators)
	for i := 0; i < numValidators; i++ {
		val, privVal := types.RandValidator(randPower, minPower)
		validators[i] = types.GenesisValidator{
			PubKey: val.PubKey,
			Power:  val.VotingPower,
		}
		privValidators[i] = privVal
	}
	sort.Sort(types.PrivValidatorsByAddress(privValidators))

	return &types.GenesisDoc{
		GenesisTime: tmtime.Now(),
		ChainID:     config.ChainID(),
		Validators:  validators,
	}, privValidators
}

func Vote(
	header *types.Header,
	blockID types.BlockID,
	valset *types.ValidatorSet,
	privVal types.PrivValidator) *types.Vote {

	pubKey, err := privVal.GetPubKey()
	require.NoError(t, err)

	valIdx, _ := valset.GetByAddress(pubKey.Address())
	vote := &types.Vote{
		ValidatorAddress: pubKey.Address(),
		ValidatorIndex:   valIdx,
		Height:           header.Height,
		Round:            1,
		Timestamp:        tmtime.Now(),
		Type:             types.PrecommitType,
		BlockID:          blockID,
	}

	_ = privVal.SignVote(header.ChainID, vote)

	return vote
}

func ProxyApp() *proxy.proxyAppConn {
	app := &SApp{}
	cc := proxy.NewLocalClientCreator(app)
	proxyApp := proxy.NewAppConns(cc)
	err := proxyApp.Start()
	if err != nil {
		panic(errors.Wrap(err, "error start app"))
	}
	return proxyApp
}

func GetBlockState(
	genesis *types.Genesis,
	consensus proxy.AppConnConsensus,
	privateValidators []types.PrivValidator,
	maxBlockHeight int64,
	fastSync bool) *SBlockState {

	blockDB := dbm.NewMemDB()
	stateDB := dbm.NewMemDB()
	blockStore := store.NewBlockStore(blockDB)

	state, err := sm.LoadStateFromDBOrGenesisDoc(stateDB, genesis)
	if err != nil {
		panic(errors.Wrap(err, "error constructing state from genesis file"))
	}

	db := dbm.NewMemDB()
	sm.SaveState(db, state)

	blockExecutor := sm.NewBlockExecutor(
		db,
		log.TestingLogger(),
		consensus,
		mock.Mempool{},
		sm.MockEvidencePool{},
	)

	for blockHeight := int64(1); blockHeight <= maxBlockHeight; blockHeight++ {
		lastCommit := types.NewCommit(blockHeight-1, 1, types.BlockID{}, nil)
		if blockHeight > 1 {
			lastBlockMeta := blockStore.LoadBlockMeta(blockHeight - 1)
			lastBlock := blockStore.LoadBlock(blockHeight - 1)

			vote := Vote(t, &lastBlock.Header, lastBlockMeta.BlockID, state.Validators, privateValidators[0])
			lastCommit = types.NewCommit(vote.Height, vote.Round, lastBlockMeta.BlockID, []types.CommitSig{vote.CommitSig()})
		}

		thisBlock := Block(blockHeight, state, lastCommit)
		thisParts := thisBlock.MakePartSet(types.BlockPartSizeBytes)

		blockID := types.BlockID{Hash: thisBlock.Hash(), PartsHeader: thisParts.Header()}

		state, _, err = blockExec.ApplyBlock(state, blockID, thisBlock)
		if err != nil {
			panic(errors.Wrap(err, "error apply block"))
		}

		blockStore.SaveBlock(thisBlock, thisParts, lastCommit)
	}

	return &SBlockState{
		state:    state.Copy(),
		store:    blockStore,
		executor: blockExecutor,
	}
}

func BlockchainReactor(
	logger log.Logger,
	bcBlockState *SBlockState,
	fastSync bool,
) *BlockchainReactor {
	bcReactor := NewBlockchainReactor(bcBlockState.state, bcBlockState.Executor, bcBlockState.store, fastSync)
	bcReactor.SetLogger(logger.With("module", "blockchain"))
	return bcReactor
}

func GetBlockchainReactor(
	logger log.Logger,
	genDoc *types.GenesisDoc,
	privVals []types.PrivValidator,
	maxBlockHeight int64,
	fastSync bool,
) *BlockchainReactor {
	if len(privVals) != 1 {
		panic("only support one validator")
	}

	app := ProxyApp()
	blockState := BlockStore(genDoc, app.Consensus(), privVals, maxBlockHeight)
	bcr := BlockChainReactor(logger, blockState, fastSync)

	return bcr
}

func GetBlockchainReactors(
	logger log.Logger,
	genDoc *types.GenesisDoc,
	privVals []types.PrivValidator,
	maxBlockHeight int64,
	fastSync bool,
) SBlockchainReactors {

	conR := &SConsensusReactor{}
	conR.BaseReactor = *p2p.NewBaseReactor("Consensus reactor", conR)

	return SBlockchainReactors{
		GetBlockchainReactor(logger, genDoc, privVals, maxBlockHeight, fastSync),
		conR,
	}
}

func SimulateFastSyncBadBlockStopsPeer() {
	numNodes := 4
	maxBlockHeight := int64(148)
	fastSync := true
	config = cfg.ResetTestRoot("blockchain_reactor_test")
	genDoc, privVals := randGenesisDoc(1, false, 30)

	defer os.RemoveAll(config.RootDir)

	reactors := make([]SBlockchainReactors, numNodes)
	logger := make([]log.Logger, numNodes)

	// create numNodes blockchain reactors
	for i := 0; i < numNodes; i++ {
		logger[i] = log.TestingLogger()
		height := int64(0)
		if i == 0 {
			height = maxBlockHeight
		}
		reactors[i] = GetBlockchainReactors(t, logger[i], genDoc, privVals, height, fastSync)
	}

	LinkNodes := func(i int, s *p2p.Switch) *p2p.Switch {
		reactors[i].conR.mtx.Lock()
		defer reactors[i].conR.mtx.Unlock()

		s.AddReactor("BLOCKCHAIN", reactors[i].bcR)
		s.AddReactor("CONSENSUS", reactors[i].conR)

		reactors[i].bcR.SetLogger(logger[i].With("module", fmt.Sprintf("blockchain-%v", i)))

		return s
	}

	// link them together to achieve connected peers behavior
	switches := p2p.MakeConnectedSwitches(config.P2P, numNodes, LinkNodes, p2p.Connect2Switches)

	defer func() {
		for _, r := range reactors {
			_ = r.bcR.Stop()
			_ = r.conR.Stop()
		}
	}()

outerFor:
	for {
		time.Sleep(10 * time.Millisecond)
		for i := 0; i < numNodes; i++ {
			reactors[i].conR.mtx.Lock()
			if !reactors[i].conR.switchedToConsensus {
				reactors[i].conR.mtx.Unlock()
				continue outerFor
			}
			reactors[i].conR.mtx.Unlock()
		}
		break
	}

	//at this time, reactors[0-3] is the newest
	assert.Equal(t, numNodes-1, reactors[1].bcR.Switch.Peers().Size())

	//mark last reactorPair as an invalid peer
	//reactors[numNodes-1].bcR.store = otherChain.bcR.store

	// create and link a new reactor
	newReactor := GetBlockchainReactors(t, log.TestingLogger(), genDoc, privVals, 0)
	reactors = append(reactors, newReactor)

	switches = append(switches, p2p.MakeConnectedSwitches(config.P2P, 1, func(i int, s *p2p.Switch) *p2p.Switch {
		r := reactors[len(reactors)-1]

		s.AddReactor("BLOCKCHAIN", r.bcR)
		s.AddReactor("CONSENSUS", r.conR)

		moduleName := fmt.Sprintf("blockchain-%v", r.bcR)

		r.bcR.SetLogger(lastLogger.With("module", moduleName))

		return s

	}, p2p.Connect2Switches)...)

	for i := 0; i < len(reactors)-1; i++ {
		p2p.Connect2Switches(switches, i, len(reactors)-1)
	}

	for {
		time.Sleep(1 * time.Second)
		newReactor.conR.mtx.Lock()
		if newReactor.conR.switchedToConsensus {
			newReactor.conR.mtx.Unlock()
			break
		}
		newReactor.conR.mtx.Unlock()

		if newReactor.bcR.Switch.Peers().Size() == 0 {
			break
		}
	}

	assert.True(t, newReactor.bcR.Switch.Peers().Size() < len(reactors)-1)
}

func main() {
	SimulateFastSyncBadBlockStopsPeer()
}
