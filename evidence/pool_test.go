package evidence_test

import (
	"os"
	"testing"
	"time"

	mdutils "github.com/ipfs/go-merkledag/test"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/lazyledger/lazyledger-core/evidence"
	"github.com/lazyledger/lazyledger-core/evidence/mocks"
	dbm "github.com/lazyledger/lazyledger-core/libs/db"
	"github.com/lazyledger/lazyledger-core/libs/db/memdb"
	"github.com/lazyledger/lazyledger-core/libs/log"
	tmproto "github.com/lazyledger/lazyledger-core/proto/tendermint/types"
	tmversion "github.com/lazyledger/lazyledger-core/proto/tendermint/version"
	sm "github.com/lazyledger/lazyledger-core/state"
	smmocks "github.com/lazyledger/lazyledger-core/state/mocks"
	"github.com/lazyledger/lazyledger-core/store"
	"github.com/lazyledger/lazyledger-core/types"
	"github.com/lazyledger/lazyledger-core/version"
)

func TestMain(m *testing.M) {

	code := m.Run()
	os.Exit(code)
}

const evidenceChainID = "test_chain"

var (
	defaultEvidenceTime           = time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	defaultEvidenceMaxBytes int64 = 1000
)

func TestEvidencePoolBasic(t *testing.T) {
	var (
		height     = int64(1)
		stateStore = &smmocks.Store{}
		evidenceDB = memdb.NewDB()
		blockStore = &mocks.BlockStore{}
	)

	valSet, privVals := types.RandValidatorSet(1, 10)

	blockStore.On("LoadBlockMeta", mock.AnythingOfType("int64")).Return(
		&types.BlockMeta{Header: types.Header{Time: defaultEvidenceTime}},
	)
	stateStore.On("LoadValidators", mock.AnythingOfType("int64")).Return(valSet, nil)
	stateStore.On("Load").Return(createState(height+1, valSet), nil)

	pool, err := evidence.NewPool(evidenceDB, stateStore, blockStore)
	require.NoError(t, err)
	pool.SetLogger(log.TestingLogger())

	// evidence not seen yet:
	evs, size := pool.PendingEvidence(defaultEvidenceMaxBytes)
	assert.Equal(t, 0, len(evs))
	assert.Zero(t, size)

	ev := types.NewMockDuplicateVoteEvidenceWithValidator(height, defaultEvidenceTime, privVals[0], evidenceChainID)

	// good evidence
	evAdded := make(chan struct{})
	go func() {
		<-pool.EvidenceWaitChan()
		close(evAdded)
	}()

	// evidence seen but not yet committed:
	assert.NoError(t, pool.AddEvidence(ev))

	select {
	case <-evAdded:
	case <-time.After(5 * time.Second):
		t.Fatal("evidence was not added to list after 5s")
	}

	next := pool.EvidenceFront()
	assert.Equal(t, ev, next.Value.(types.Evidence))

	const evidenceBytes int64 = 372
	evs, size = pool.PendingEvidence(evidenceBytes)
	assert.Equal(t, 1, len(evs))
	assert.Equal(t, evidenceBytes, size) // check that the size of the single evidence in bytes is correct

	// shouldn't be able to add evidence twice
	assert.NoError(t, pool.AddEvidence(ev))
	evs, _ = pool.PendingEvidence(defaultEvidenceMaxBytes)
	assert.Equal(t, 1, len(evs))

}

// Tests inbound evidence for the right time and height
func TestAddExpiredEvidence(t *testing.T) {
	var (
		val                 = types.NewMockPV()
		height              = int64(30)
		stateStore          = initializeValidatorState(val, height)
		evidenceDB          = memdb.NewDB()
		blockStore          = &mocks.BlockStore{}
		expiredEvidenceTime = time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC)
		expiredHeight       = int64(2)
	)

	blockStore.On("LoadBlockMeta", mock.AnythingOfType("int64")).Return(func(h int64) *types.BlockMeta {
		if h == height || h == expiredHeight {
			return &types.BlockMeta{Header: types.Header{Time: defaultEvidenceTime}}
		}
		return &types.BlockMeta{Header: types.Header{Time: expiredEvidenceTime}}
	})

	pool, err := evidence.NewPool(evidenceDB, stateStore, blockStore)
	require.NoError(t, err)

	testCases := []struct {
		evHeight      int64
		evTime        time.Time
		expErr        bool
		evDescription string
	}{
		{height, defaultEvidenceTime, false, "valid evidence"},
		{expiredHeight, defaultEvidenceTime, false, "valid evidence (despite old height)"},
		{height - 1, expiredEvidenceTime, false, "valid evidence (despite old time)"},
		{expiredHeight - 1, expiredEvidenceTime, true,
			"evidence from height 1 (created at: 2019-01-01 00:00:00 +0000 UTC) is too old"},
		{height, defaultEvidenceTime.Add(1 * time.Minute), true, "evidence time and block time is different"},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.evDescription, func(t *testing.T) {
			ev := types.NewMockDuplicateVoteEvidenceWithValidator(tc.evHeight, tc.evTime, val, evidenceChainID)
			err := pool.AddEvidence(ev)
			if tc.expErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestAddEvidenceFromConsensus(t *testing.T) {
	var height int64 = 10
	pool, val := defaultTestPool(height)
	ev := types.NewMockDuplicateVoteEvidenceWithValidator(height, defaultEvidenceTime, val, evidenceChainID)
	err := pool.AddEvidenceFromConsensus(ev)
	assert.NoError(t, err)
	next := pool.EvidenceFront()
	assert.Equal(t, ev, next.Value.(types.Evidence))

	// shouldn't be able to submit the same evidence twice
	err = pool.AddEvidenceFromConsensus(ev)
	assert.NoError(t, err)
	evs, _ := pool.PendingEvidence(defaultEvidenceMaxBytes)
	assert.Equal(t, 1, len(evs))
}

func TestEvidencePoolUpdate(t *testing.T) {
	height := int64(21)
	pool, val := defaultTestPool(height)
	state := pool.State()

	// create new block (no need to save it to blockStore)
	prunedEv := types.NewMockDuplicateVoteEvidenceWithValidator(1, defaultEvidenceTime.Add(1*time.Minute),
		val, evidenceChainID)
	err := pool.AddEvidence(prunedEv)
	require.NoError(t, err)
	ev := types.NewMockDuplicateVoteEvidenceWithValidator(height, defaultEvidenceTime.Add(21*time.Minute),
		val, evidenceChainID)
	lastCommit := makeCommit(height, val.PrivKey.PubKey().Address())
	block := types.MakeBlock(height+1, []types.Tx{}, []types.Evidence{ev}, nil, types.Messages{}, lastCommit)
	// update state (partially)
	state.LastBlockHeight = height + 1
	state.LastBlockTime = defaultEvidenceTime.Add(22 * time.Minute)
	err = pool.CheckEvidence(types.EvidenceList{ev})
	require.NoError(t, err)

	pool.Update(state, block.Evidence.Evidence)
	// a) Update marks evidence as committed so pending evidence should be empty
	evList, evSize := pool.PendingEvidence(defaultEvidenceMaxBytes)
	assert.Empty(t, evList)
	assert.Zero(t, evSize)

	// b) If we try to check this evidence again it should fail because it has already been committed
	err = pool.CheckEvidence(types.EvidenceList{ev})
	if assert.Error(t, err) {
		assert.Equal(t, "evidence was already committed", err.(*types.ErrInvalidEvidence).Reason.Error())
	}
}

func TestVerifyPendingEvidencePasses(t *testing.T) {
	var height int64 = 1
	pool, val := defaultTestPool(height)
	ev := types.NewMockDuplicateVoteEvidenceWithValidator(height, defaultEvidenceTime.Add(1*time.Minute),
		val, evidenceChainID)
	err := pool.AddEvidence(ev)
	require.NoError(t, err)

	err = pool.CheckEvidence(types.EvidenceList{ev})
	assert.NoError(t, err)
}

func TestVerifyDuplicatedEvidenceFails(t *testing.T) {
	var height int64 = 1
	pool, val := defaultTestPool(height)
	ev := types.NewMockDuplicateVoteEvidenceWithValidator(height, defaultEvidenceTime.Add(1*time.Minute),
		val, evidenceChainID)
	err := pool.CheckEvidence(types.EvidenceList{ev, ev})
	if assert.Error(t, err) {
		assert.Equal(t, "duplicate evidence", err.(*types.ErrInvalidEvidence).Reason.Error())
	}
}

// check that valid light client evidence is correctly validated and stored in
// evidence pool
func TestCheckEvidenceWithLightClientAttack(t *testing.T) {
	var (
		nValidators          = 5
		validatorPower int64 = 10
		height         int64 = 10
	)
	conflictingVals, conflictingPrivVals := types.RandValidatorSet(nValidators, validatorPower)
	trustedHeader := makeHeaderRandom(height)
	trustedHeader.Time = defaultEvidenceTime

	conflictingHeader := makeHeaderRandom(height)
	conflictingHeader.ValidatorsHash = conflictingVals.Hash()

	trustedHeader.ValidatorsHash = conflictingHeader.ValidatorsHash
	trustedHeader.NextValidatorsHash = conflictingHeader.NextValidatorsHash
	trustedHeader.ConsensusHash = conflictingHeader.ConsensusHash
	trustedHeader.AppHash = conflictingHeader.AppHash
	trustedHeader.LastResultsHash = conflictingHeader.LastResultsHash

	// for simplicity we are simulating a duplicate vote attack where all the validators in the
	// conflictingVals set voted twice
	blockID := makeBlockID(conflictingHeader.Hash(), 1000, []byte("partshash"))
	voteSet := types.NewVoteSet(evidenceChainID, height, 1, tmproto.SignedMsgType(2), conflictingVals)
	commit, err := types.MakeCommit(blockID, height, 1, voteSet, conflictingPrivVals, defaultEvidenceTime)
	require.NoError(t, err)
	ev := &types.LightClientAttackEvidence{
		ConflictingBlock: &types.LightBlock{
			SignedHeader: &types.SignedHeader{
				Header: conflictingHeader,
				Commit: commit,
			},
			ValidatorSet: conflictingVals,
		},
		CommonHeight:        10,
		TotalVotingPower:    int64(nValidators) * validatorPower,
		ByzantineValidators: conflictingVals.Validators,
		Timestamp:           defaultEvidenceTime,
	}

	trustedBlockID := makeBlockID(trustedHeader.Hash(), 1000, []byte("partshash"))
	trustedVoteSet := types.NewVoteSet(evidenceChainID, height, 1, tmproto.SignedMsgType(2), conflictingVals)
	trustedCommit, err := types.MakeCommit(trustedBlockID, height, 1, trustedVoteSet, conflictingPrivVals,
		defaultEvidenceTime)
	require.NoError(t, err)

	state := sm.State{
		LastBlockTime:   defaultEvidenceTime.Add(1 * time.Minute),
		LastBlockHeight: 11,
		ConsensusParams: *types.DefaultConsensusParams(),
	}
	stateStore := &smmocks.Store{}
	stateStore.On("LoadValidators", height).Return(conflictingVals, nil)
	stateStore.On("Load").Return(state, nil)
	blockStore := &mocks.BlockStore{}
	blockStore.On("LoadBlockMeta", height).Return(&types.BlockMeta{Header: *trustedHeader})
	blockStore.On("LoadBlockCommit", height).Return(trustedCommit)

	pool, err := evidence.NewPool(memdb.NewDB(), stateStore, blockStore)
	require.NoError(t, err)
	pool.SetLogger(log.TestingLogger())

	err = pool.AddEvidence(ev)
	assert.NoError(t, err)

	err = pool.CheckEvidence(types.EvidenceList{ev})
	assert.NoError(t, err)

	// take away the last signature -> there are less validators then what we have detected,
	// hence this should fail
	commit.Signatures = append(commit.Signatures[:nValidators-1], types.NewCommitSigAbsent())
	err = pool.CheckEvidence(types.EvidenceList{ev})
	assert.Error(t, err)
}

// Tests that restarting the evidence pool after a potential failure will recover the
// pending evidence and continue to gossip it
func TestRecoverPendingEvidence(t *testing.T) {
	height := int64(10)
	val := types.NewMockPV()
	valAddress := val.PrivKey.PubKey().Address()
	evidenceDB := memdb.NewDB()
	stateStore := initializeValidatorState(val, height)
	state, err := stateStore.Load()
	require.NoError(t, err)
	blockStore := initializeBlockStore(memdb.NewDB(), state, valAddress)
	// create previous pool and populate it
	pool, err := evidence.NewPool(evidenceDB, stateStore, blockStore)
	require.NoError(t, err)
	pool.SetLogger(log.TestingLogger())
	goodEvidence := types.NewMockDuplicateVoteEvidenceWithValidator(height,
		defaultEvidenceTime.Add(10*time.Minute), val, evidenceChainID)
	expiredEvidence := types.NewMockDuplicateVoteEvidenceWithValidator(int64(1),
		defaultEvidenceTime.Add(1*time.Minute), val, evidenceChainID)
	err = pool.AddEvidence(goodEvidence)
	require.NoError(t, err)
	err = pool.AddEvidence(expiredEvidence)
	require.NoError(t, err)

	// now recover from the previous pool at a different time
	newStateStore := &smmocks.Store{}
	newStateStore.On("Load").Return(sm.State{
		LastBlockTime:   defaultEvidenceTime.Add(25 * time.Minute),
		LastBlockHeight: height + 15,
		ConsensusParams: tmproto.ConsensusParams{
			Block: tmproto.BlockParams{
				MaxBytes: 22020096,
				MaxGas:   -1,
			},
			Evidence: tmproto.EvidenceParams{
				MaxAgeNumBlocks: 20,
				MaxAgeDuration:  20 * time.Minute,
				MaxBytes:        1000,
			},
		},
	}, nil)
	newPool, err := evidence.NewPool(evidenceDB, newStateStore, blockStore)
	assert.NoError(t, err)
	evList, _ := newPool.PendingEvidence(defaultEvidenceMaxBytes)
	assert.Equal(t, 1, len(evList))
	next := newPool.EvidenceFront()
	assert.Equal(t, goodEvidence, next.Value.(types.Evidence))

}

func initializeStateFromValidatorSet(valSet *types.ValidatorSet, height int64) sm.Store {
	stateDB := memdb.NewDB()
	stateStore := sm.NewStore(stateDB)
	state := sm.State{
		ChainID:                     evidenceChainID,
		InitialHeight:               1,
		LastBlockHeight:             height,
		LastBlockTime:               defaultEvidenceTime,
		Validators:                  valSet,
		NextValidators:              valSet.CopyIncrementProposerPriority(1),
		LastValidators:              valSet,
		LastHeightValidatorsChanged: 1,
		ConsensusParams: tmproto.ConsensusParams{
			Block: tmproto.BlockParams{
				MaxBytes: 22020096,
				MaxGas:   -1,
			},
			Evidence: tmproto.EvidenceParams{
				MaxAgeNumBlocks: 20,
				MaxAgeDuration:  20 * time.Minute,
				MaxBytes:        1000,
			},
		},
	}

	// save all states up to height
	for i := int64(0); i <= height; i++ {
		state.LastBlockHeight = i
		if err := stateStore.Save(state); err != nil {
			panic(err)
		}
	}

	return stateStore
}

func initializeValidatorState(privVal types.PrivValidator, height int64) sm.Store {

	pubKey, _ := privVal.GetPubKey()
	validator := &types.Validator{Address: pubKey.Address(), VotingPower: 10, PubKey: pubKey}

	// create validator set and state
	valSet := &types.ValidatorSet{
		Validators: []*types.Validator{validator},
		Proposer:   validator,
	}

	return initializeStateFromValidatorSet(valSet, height)
}

// initializeBlockStore creates a block storage and populates it w/ a dummy
// block at +height+.
func initializeBlockStore(db dbm.DB, state sm.State, valAddr []byte) *store.BlockStore {
	blockStore := store.NewBlockStore(db, mdutils.Mock())

	for i := int64(1); i <= state.LastBlockHeight; i++ {
		lastCommit := makeCommit(i-1, valAddr)
		block, _ := state.MakeBlock(i, []types.Tx{}, nil, nil,
			types.Messages{}, lastCommit, state.Validators.GetProposer().Address)
		block.Header.Time = defaultEvidenceTime.Add(time.Duration(i) * time.Minute)
		block.Header.Version = tmversion.Consensus{Block: version.BlockProtocol, App: 1}
		const parts = 1
		partSet := block.MakePartSet(parts)

		seenCommit := makeCommit(i, valAddr)
		blockStore.SaveBlock(block, partSet, seenCommit)
	}

	return blockStore
}

func makeCommit(height int64, valAddr []byte) *types.Commit {
	commitSigs := []types.CommitSig{{
		BlockIDFlag:      types.BlockIDFlagCommit,
		ValidatorAddress: valAddr,
		Timestamp:        defaultEvidenceTime,
		Signature:        []byte("Signature"),
	}}
	return types.NewCommit(height, 0, types.BlockID{}, commitSigs)
}

func defaultTestPool(height int64) (*evidence.Pool, types.MockPV) {
	val := types.NewMockPV()
	valAddress := val.PrivKey.PubKey().Address()
	evidenceDB := memdb.NewDB()
	stateStore := initializeValidatorState(val, height)
	state, _ := stateStore.Load()
	blockStore := initializeBlockStore(memdb.NewDB(), state, valAddress)
	pool, err := evidence.NewPool(evidenceDB, stateStore, blockStore)
	if err != nil {
		panic("test evidence pool could not be created")
	}
	pool.SetLogger(log.TestingLogger())
	return pool, val
}

func createState(height int64, valSet *types.ValidatorSet) sm.State {
	return sm.State{
		ChainID:         evidenceChainID,
		LastBlockHeight: height,
		LastBlockTime:   defaultEvidenceTime,
		Validators:      valSet,
		ConsensusParams: *types.DefaultConsensusParams(),
	}
}
