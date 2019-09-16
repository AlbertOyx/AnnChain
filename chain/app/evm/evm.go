// Copyright © 2017 ZhongAn Technology
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package evm

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math/big"
	"path/filepath"
	"sync"

	"go.uber.org/zap"

	"github.com/pkg/errors"
	"github.com/spf13/viper"

	rtypes "github.com/dappledger/AnnChain/chain/types"
	"github.com/dappledger/AnnChain/eth/common"
	"github.com/dappledger/AnnChain/eth/common/math"
	"github.com/dappledger/AnnChain/eth/core"
	"github.com/dappledger/AnnChain/eth/core/rawdb"
	estate "github.com/dappledger/AnnChain/eth/core/state"
	etypes "github.com/dappledger/AnnChain/eth/core/types"
	"github.com/dappledger/AnnChain/eth/core/vm"
	"github.com/dappledger/AnnChain/eth/ethdb"
	"github.com/dappledger/AnnChain/eth/params"
	"github.com/dappledger/AnnChain/eth/rlp"
	"github.com/dappledger/AnnChain/gemmill/modules/go-log"
	"github.com/dappledger/AnnChain/gemmill/modules/go-merkle"
	gtypes "github.com/dappledger/AnnChain/gemmill/types"
	"github.com/dappledger/AnnChain/utils/commu"
	"github.com/dappledger/AnnChain/utils/private"
)

const (
	AppName         = "evm"
	DatabaseCache   = 128
	DatabaseHandles = 1024

	// With 2.2 GHz Intel Core i7, 16 GB 2400 MHz DDR4, 256GB SSD, we tested following contract, it takes about 24157 gas and 171.193µs.
	// function setVal(uint256 _val) public {
	//	val = _val;
	//	emit SetVal(_val,_val);
	//  emit SetValByWho("a name which length is bigger than 32 bytes",msg.sender, _val);
	// }
	// So we estimate that running out of 100000000 gas may be taken at least 1s to 10s
	EVMGasLimit uint64 = 100000000
)

//reference ethereum BlockChain
type BlockChainEvm struct {
	db ethdb.Database
}

func NewBlockChain(db ethdb.Database) *BlockChainEvm {
	return &BlockChainEvm{db}
}

func (bc *BlockChainEvm) GetHeader(hash common.Hash, number uint64) *etypes.Header {
	//todo cache,reference core/headerchain.go
	header := rawdb.ReadHeader(bc.db, hash, number)
	if header == nil {
		return nil
	}
	return header
}

var (
	ReceiptsPrefix = []byte("receipts-")

	EmptyTrieRoot = common.HexToHash("56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")

	IsHomestead = true
	evmConfig   = vm.Config{EVMGasLimit: EVMGasLimit}

	errQuitExecute = fmt.Errorf("quit executing block")
)

type EVMApp struct {
	pool *ethTxPool
	gtypes.BaseApplication
	AngineHooks gtypes.Hooks

	core gtypes.Core

	datadir string
	Config  *viper.Viper

	secChanHost string

	currentHeader *etypes.Header
	chainConfig   *params.ChainConfig

	stateDb  ethdb.Database
	stateMtx sync.Mutex

	publicState        *estate.StateDB
	currentPublicState *estate.StateDB

	privateState        *estate.StateDB
	currentPrivateState *estate.StateDB

	publicReceipts  etypes.Receipts
	privateReceipts etypes.Receipts

	publicSigner  etypes.Signer
	privateSigner etypes.Signer
}

type LastBlockInfo struct {
	Height   int64
	AppHash  []byte
	PrivHash []byte
}

func NewEVMApp(config *viper.Viper) (*EVMApp, error) {
	app := &EVMApp{
		datadir:       config.GetString("db_dir"),
		secChanHost:   config.GetString("private_server_host"),
		Config:        config,
		chainConfig:   params.MainnetChainConfig,
		publicSigner:  new(etypes.HomesteadSigner),
		privateSigner: new(etypes.AnnsteadSigner),
	}

	app.AngineHooks = gtypes.Hooks{
		OnNewRound: gtypes.NewHook(app.OnNewRound),
		OnCommit:   gtypes.NewHook(app.OnCommit),
		OnPrevote:  gtypes.NewHook(app.OnPrevote),
		OnExecute:  gtypes.NewHook(app.OnExecute),
	}

	var err error
	if err = app.BaseApplication.InitBaseApplication(AppName, app.datadir); err != nil {
		log.Error("InitBaseApplication error", zap.Error(err))
		return nil, errors.Wrap(err, "app error")
	}

	if app.stateDb, err = OpenDatabase(app.datadir, "chaindata", DatabaseCache, DatabaseHandles); err != nil {
		log.Error("OpenDatabase error", zap.Error(err))
		return nil, errors.Wrap(err, "app error")
	}

	app.pool = NewEthTxPool(app, config)

	return app, nil
}

func OpenDatabase(datadir string, name string, cache int, handles int) (ethdb.Database, error) {
	return ethdb.NewLDBDatabase(filepath.Join(datadir, name), cache, handles)
}

func (app *EVMApp) writeGenesis() error {
	pubHash, privHash := app.getLastAppHash()
	if pubHash != EmptyTrieRoot && privHash != EmptyTrieRoot {
		return nil
	}

	g := core.DefaultGenesis()
	b := g.ToBlock(app.stateDb)
	app.SaveLastBlock(LastBlockInfo{Height: 0, AppHash: b.Root().Bytes(), PrivHash: b.Root().Bytes()})
	return nil
}

func (app *EVMApp) Start() (err error) {
	if err := app.writeGenesis(); err != nil {
		app.Stop()
		log.Error("write genesis err:", zap.Error(err))
		return err
	}

	lastBlock := &LastBlockInfo{
		Height:   0,
		AppHash:  make([]byte, 0),
		PrivHash: make([]byte, 0),
	}
	if res, err := app.LoadLastBlock(lastBlock); err == nil && res != nil {
		lastBlock = res.(*LastBlockInfo)
	}
	if err != nil {
		log.Error("fail to load last block", zap.Error(err))
		return
	}

	// Load evm publicState when starting
	pubTrieRoot := EmptyTrieRoot
	if len(lastBlock.AppHash) > 0 {
		pubTrieRoot = common.BytesToHash(lastBlock.AppHash)
	}
	privTrieRoot := EmptyTrieRoot
	if len(lastBlock.PrivHash) > 0 {
		privTrieRoot = common.BytesToHash(lastBlock.PrivHash)
	}
	app.pool.Start(lastBlock.Height)
	if app.publicState, err = estate.New(pubTrieRoot, estate.NewDatabase(app.stateDb)); err != nil {
		app.Stop()
		log.Error("fail to new publicState", zap.Error(err))
		return
	}
	if app.privateState, err = estate.New(privTrieRoot, estate.NewDatabase(app.stateDb)); err != nil {
		app.Stop()
		log.Error("fail to new privateState", zap.Error(err))
		return
	}
	commu.DefaultHost = app.secChanHost
	return nil
}

func (app *EVMApp) getLastAppHash() (pubHash, privHash common.Hash) {
	lastBlock := &LastBlockInfo{
		Height:   0,
		AppHash:  make([]byte, 0),
		PrivHash: make([]byte, 0),
	}
	if res, err := app.LoadLastBlock(lastBlock); err == nil && res != nil {
		lastBlock = res.(*LastBlockInfo)
	}
	if len(lastBlock.AppHash) > 0 {
		pubHash = common.BytesToHash(lastBlock.AppHash)
	} else {
		pubHash = EmptyTrieRoot
	}
	if len(lastBlock.PrivHash) > 0 {
		privHash = common.BytesToHash(lastBlock.PrivHash)
	} else {
		privHash = EmptyTrieRoot
	}
	return
}

func (app *EVMApp) GetTxPool() gtypes.TxPool {
	return app.pool
}

func (app *EVMApp) Stop() {
	app.BaseApplication.Stop()
	app.stateDb.Close()
}

func (app *EVMApp) GetAngineHooks() gtypes.Hooks {
	return app.AngineHooks
}

func (app *EVMApp) CompatibleWithAngine() {}

func (app *EVMApp) BeginExecute() {}

func (app *EVMApp) OnNewRound(height, round int64, block *gtypes.Block) (interface{}, error) {
	return gtypes.NewRoundResult{}, nil
}

func (app *EVMApp) OnPrevote(height, round int64, block *gtypes.Block) (interface{}, error) {
	return nil, nil
}

func (app *EVMApp) makePublicPayloadHashReceipt(tx *etypes.Transaction) *etypes.Receipt {

	return &etypes.Receipt{
		TxHash:  tx.Hash(),
		GasUsed: 0,
		Status:  1,
	}
}

func (app *EVMApp) genExecFun(block *gtypes.Block, res *gtypes.ExecuteResult) BeginExecFunc {
	blockHash := common.BytesToHash(block.Hash())
	app.currentHeader = makeCurrentHeader(block, block.Header)

	return func() (ExecFunc, EndExecFunc) {
		publicState := app.currentPublicState
		publicSnapshot := publicState.Snapshot()

		privateState := app.currentPrivateState
		privateSnapshot := privateState.Snapshot()

		temPublicReceipt := make([]*etypes.Receipt, 0)
		temPrivateReceipt := make([]*etypes.Receipt, 0)

		execFunc := func(txIndex int, raw []byte, tx *etypes.Transaction) error {
			gp := new(core.GasPool).AddGas(math.MaxBig256.Uint64())
			txBytes, err := rlp.EncodeToBytes(tx)
			if err != nil {
				return err
			}
			txhash := gtypes.Tx(txBytes).Hash()
			var runEvmState *estate.StateDB
			from, _ := app.publicSigner.Sender(tx)
			if tx.Protected() {
				if len(app.secChanHost) > 0 {
					runEvmState = privateState
					publicState.SetNonce(from, publicState.GetNonce(from)+1)
					fmt.Println("===privatestate begin evm tx", from.Hex())
				} else {
					publicState.Prepare(common.BytesToHash(txhash), blockHash, txIndex)
					publicState.SetNonce(from, publicState.GetNonce(from)+1)
					privateState.SetNonce(from, privateState.GetNonce(from)+1)
					temPublicReceipt = append(temPublicReceipt, app.makePublicPayloadHashReceipt(tx))
					return nil
				}

			} else {
				runEvmState = publicState
				privateState.SetNonce(from, privateState.GetNonce(from)+1)
				fmt.Println("===publicstate begin evm tx")
			}

			runEvmState.Prepare(common.BytesToHash(txhash), blockHash, txIndex)

			bc := NewBlockChain(app.stateDb)
			receipt, _, err := core.ApplyTransaction(
				app.chainConfig,
				bc,
				nil, // coinbase ,maybe use local account
				gp,
				runEvmState,
				app.currentHeader,
				tx,
				new(uint64),
				evmConfig)

			if err != nil {
				return err
			}
			if tx.Protected() {
				temPrivateReceipt = append(temPrivateReceipt, receipt)
			} else {
				temPublicReceipt = append(temPublicReceipt, receipt)
			}
			return nil
		}

		endFunc := func(raw []byte, err error) bool {
			if err != nil {
				log.Warn("[evm execute],apply transaction", zap.Error(err))
				publicState.RevertToSnapshot(publicSnapshot)
				privateState.RevertToSnapshot(privateSnapshot)
				temPrivateReceipt = nil
				temPublicReceipt = nil
				res.InvalidTxs = append(res.InvalidTxs, gtypes.ExecuteInvalidTx{Bytes: raw, Error: err})
				return true
			}
			app.privateReceipts = append(app.privateReceipts, temPrivateReceipt...)
			app.publicReceipts = append(app.publicReceipts, temPublicReceipt...)
			res.ValidTxs = append(res.ValidTxs, raw)
			return true
		}
		return execFunc, endFunc
	}
}

func makeCurrentHeader(block *gtypes.Block, header *gtypes.Header) *etypes.Header {
	return &etypes.Header{
		ParentHash: common.BytesToHash(block.Header.LastBlockID.Hash),
		Difficulty: big.NewInt(0),
		GasLimit:   math.MaxBig256.Uint64(),
		Time:       big.NewInt(block.Header.Time.Unix()),
		Number:     big.NewInt(header.Height),
	}
}

func (app *EVMApp) OnExecute(height, round int64, block *gtypes.Block) (interface{}, error) {
	var (
		res gtypes.ExecuteResult
		err error
	)
	fmt.Println("onExecute tx:", height)
	pubHash, privHash := app.getLastAppHash()
	if app.currentPublicState, err = estate.New(pubHash, estate.NewDatabase(app.stateDb)); err != nil {
		return nil, errors.Wrap(err, "create StateDB failed")
	}
	if app.currentPrivateState, err = estate.New(privHash, estate.NewDatabase(app.stateDb)); err != nil {
		return nil, errors.Wrap(err, "create StateDB failed")
	}
	exeWithCPUParallelVeirfy(app.publicSigner, app.privateSigner, block.Data.Txs, nil, app.genExecFun(block, &res))

	m := make(map[string]int)
	for _, tx := range block.Data.Txs {
		m[string(tx)]++
	}
	dups := 0
	for _, v := range m {
		if v > 1 {
			dups++
		}
	}
	return res, err
}

// OnCommit run in a sync way, we don't need to lock stateDupMtx, but stateMtx is still needed
func (app *EVMApp) OnCommit(height, round int64, block *gtypes.Block) (interface{}, error) {
	appHash, err := app.currentPublicState.Commit(true)
	if err != nil {
		return nil, err
	}
	privHash, err := app.currentPrivateState.Commit(true)
	if err != nil {
		return nil, err
	}

	if err := app.currentPublicState.Database().TrieDB().Commit(appHash, false); err != nil {
		return nil, err
	}
	if err := app.currentPrivateState.Database().TrieDB().Commit(privHash, false); err != nil {
		return nil, err
	}

	app.stateMtx.Lock()
	if app.publicState, err = estate.New(appHash, estate.NewDatabase(app.stateDb)); err != nil {
		app.stateMtx.Unlock()
		return nil, errors.Wrap(err, "create public StateDB failed")
	}
	if app.privateState, err = estate.New(privHash, estate.NewDatabase(app.stateDb)); err != nil {
		app.stateMtx.Unlock()
		return nil, errors.Wrap(err, "create private StateDB failed")
	}
	app.stateMtx.Unlock()

	app.SaveLastBlock(LastBlockInfo{Height: height, AppHash: appHash.Bytes(), PrivHash: privHash.Bytes()})

	rHash, err := app.SaveReceipts()
	if err != nil {
		log.Error("application save receipts", zap.Error(err), zap.Int64("height", block.Height))
	}

	app.publicReceipts = nil
	app.privateReceipts = nil
	app.pool.updateToState()
	log.Info("application save to db", zap.String("appHash", fmt.Sprintf("%X", appHash.Bytes())), zap.String("receiptHash", fmt.Sprintf("%X", rHash)))

	return gtypes.CommitResult{
		AppHash:      appHash.Bytes(),
		ReceiptsHash: rHash,
	}, nil
}

func (app *EVMApp) CheckTx(bs []byte) ([]byte, error) {
	tx := &etypes.Transaction{}
	err := rlp.DecodeBytes(bs, tx)
	if err != nil {
		return nil, err
	}
	var (
		judgeState *estate.StateDB
		from       common.Address
	)
	if tx.Protected() {
		from, _ = etypes.Sender(app.privateSigner, tx)
		judgeState = app.privateState
		repPayload := new(private.ReplacePayload)
		if err := repPayload.Decode(tx.Data()); err != nil {
			return nil, err
		}
		if len(app.secChanHost) > 0 {
			payloadHash, err := commu.SendPayload("", repPayload.PrivateMembers, repPayload.Payload)
			if err != nil {
				return nil, err
			}
			tx.SetData(payloadHash)
			fmt.Println("SendPayload Success:", common.Bytes2Hex(payloadHash), repPayload.Payload)
		} else {
			return nil, errors.New("node private tx unsupported")
		}

	} else {
		from, _ = etypes.Sender(app.publicSigner, tx)
		judgeState = app.publicState
	}
	app.stateMtx.Lock()
	defer app.stateMtx.Unlock()
	// Last but not least check for nonce errors
	nonce := tx.Nonce()
	getNonce := judgeState.GetNonce(from)
	if getNonce > nonce {
		txhash := gtypes.Tx(bs).Hash()
		return nil, fmt.Errorf("nonce(%d) different with getNonce(%d), transaction already exists %v", nonce, getNonce, hex.EncodeToString(txhash))
	}
	// Transactor should have enough funds to cover the costs
	// cost == V + GP * GL
	if judgeState.GetBalance(from).Cmp(tx.Cost()) < 0 {
		return nil, fmt.Errorf("not enough funds")
	}
	return rlp.EncodeToBytes(tx)
}

func (app *EVMApp) SaveReceipts() ([]byte, error) {
	savedReceipts := make([][]byte, 0, len(app.publicReceipts))
	receiptBatch := app.stateDb.NewBatch()

	for _, publicReceipt := range app.publicReceipts {
		storageReceipt := (*etypes.ReceiptForStorage)(publicReceipt)
		storageReceiptBytes, err := rlp.EncodeToBytes(storageReceipt)
		if err != nil {
			return nil, fmt.Errorf("wrong rlp encode:%v", err.Error())
		}

		key := append(ReceiptsPrefix, publicReceipt.TxHash.Bytes()...)
		if err := receiptBatch.Put(key, storageReceiptBytes); err != nil {
			return nil, fmt.Errorf("batch publicReceipt failed:%v", err.Error())
		}
		savedReceipts = append(savedReceipts, storageReceiptBytes)
	}

	for _, privReceipt := range app.privateReceipts {
		storageReceipt := (*etypes.ReceiptForStorage)(privReceipt)
		storageReceiptBytes, err := rlp.EncodeToBytes(storageReceipt)
		if err != nil {
			return nil, fmt.Errorf("wrong rlp encode:%v", err.Error())
		}

		key := append(ReceiptsPrefix, privReceipt.TxHash.Bytes()...)
		if err := receiptBatch.Put(key, storageReceiptBytes); err != nil {
			return nil, fmt.Errorf("batch storageReceipt failed:%v", err.Error())
		}
	}

	if err := receiptBatch.Write(); err != nil {
		return nil, fmt.Errorf("persist receipts failed:%v", err.Error())
	}
	rHash := merkle.SimpleHashFromHashes(savedReceipts)
	return rHash, nil
}

func (app *EVMApp) Info() (resInfo gtypes.ResultInfo) {
	lb := &LastBlockInfo{
		AppHash: make([]byte, 0),
		Height:  0,
	}
	if res, err := app.LoadLastBlock(lb); err == nil {
		lb = res.(*LastBlockInfo)
	}

	resInfo.LastBlockAppHash = lb.AppHash
	resInfo.LastBlockHeight = lb.Height
	resInfo.Version = "alpha 0.2"
	resInfo.Data = "default app with evm-1.5.9"
	return
}

func (app *EVMApp) Query(query []byte) (res gtypes.Result) {
	action := query[0]
	load := query[1:]
	switch action {
	case rtypes.QueryType_Contract:
		res = app.queryContract(load, 0)
	case rtypes.QueryTypeContractByHeight:
		if len(load) < 8 {
			return gtypes.NewError(gtypes.CodeType_BaseInvalidInput, "wrong height")
		}
		h := binary.BigEndian.Uint64(load[len(load)-8:])
		res = app.queryContract(load[:len(load)-8], h)
	case rtypes.QueryType_Nonce:
		res = app.queryNonce(load)
	case rtypes.QueryType_Receipt:
		res = app.queryReceipt(load)
	case rtypes.QueryType_Existence:
		res = app.queryContractExistence(load)
	case rtypes.QueryType_PayLoad:
		res = app.queryPayLoad(load)
	case rtypes.QueryType_TxRaw:
		res = app.queryTransaction(load)
	default:
		res = gtypes.NewError(gtypes.CodeType_BaseInvalidInput, "unimplemented query")
	}

	// check if contract exists
	return res
}

func (app *EVMApp) queryContractExistence(load []byte) gtypes.Result {
	tx := new(etypes.Transaction)
	err := rlp.DecodeBytes(load, tx)
	if err != nil {
		return gtypes.NewError(gtypes.CodeType_BaseInvalidInput, err.Error())
	}
	contractAddr := tx.To()
	var hashBytes []byte
	app.stateMtx.Lock()
	if tx.Protected() {
		hashBytes = app.privateState.GetCodeHash(*contractAddr).Bytes()
	} else {
		hashBytes = app.publicState.GetCodeHash(*contractAddr).Bytes()
	}
	app.stateMtx.Unlock()

	if bytes.Equal(tx.Data(), hashBytes) {
		return gtypes.NewResultOK(append([]byte{}, byte(0x01)), "contract exists")
	}
	return gtypes.NewResultOK(append([]byte{}, byte(0x00)), "constract doesn't exist")
}

func (app *EVMApp) Sender(tx *etypes.Transaction) (common.Address, error) {
	if tx.Protected() {
		return app.privateSigner.Sender(tx)
	}
	return app.publicSigner.Sender(tx)
}

func (app *EVMApp) queryContract(load []byte, height uint64) gtypes.Result {
	tx := new(etypes.Transaction)
	err := rlp.DecodeBytes(load, tx)
	if err != nil {
		return gtypes.NewError(gtypes.CodeType_BaseInvalidInput, err.Error())
	}

	from, err := app.Sender(tx)
	if err != nil {
		return gtypes.NewError(gtypes.CodeType_BaseInvalidInput, err.Error())
	}
	txMsg := etypes.NewMessage(from, tx.To(), 0, tx.Value(), tx.Gas(), tx.GasPrice(), tx.Data(), false)

	bc := NewBlockChain(app.stateDb)

	var (
		vmEnv *vm.EVM
	)

	if height == 0 {

		envCxt := core.NewEVMContext(txMsg, app.currentHeader, bc, nil)

		fmt.Println("====isPrivateTx", tx.Protected())

		app.stateMtx.Lock()
		if tx.Protected() {
			vmEnv = vm.NewEVM(envCxt, app.currentPrivateState.Copy(), app.chainConfig, evmConfig)
		} else {
			vmEnv = vm.NewEVM(envCxt, app.currentPublicState.Copy(), app.chainConfig, evmConfig)
		}

		app.stateMtx.Unlock()
	} else {
		//appHash save in next block AppHash
		height++
		blockMeta, err := app.core.GetBlockMeta(int64(height))
		if err != nil {
			return gtypes.NewError(gtypes.CodeType_BaseInvalidInput, err.Error())
		}
		ethHeader := makeETHHeader(blockMeta.Header)
		envCxt := core.NewEVMContext(txMsg, ethHeader, bc, nil)

		trieRoot := EmptyTrieRoot
		if len(blockMeta.Header.AppHash) > 0 {
			trieRoot = common.BytesToHash(blockMeta.Header.AppHash)
		}

		evmState, err := estate.New(trieRoot, estate.NewDatabase(app.stateDb))
		if err != nil {
			return gtypes.NewError(gtypes.CodeType_BaseInvalidInput, err.Error())
		}
		vmEnv = vm.NewEVM(envCxt, evmState, app.chainConfig, evmConfig)
	}

	gpl := new(core.GasPool).AddGas(math.MaxBig256.Uint64())
	res, _, _, err := core.ApplyMessage(vmEnv, txMsg, gpl) // we don't care about gasUsed
	if err != nil {
		log.Warn("query apply msg err", zap.Error(err))
	}
	fmt.Println("====res", res)
	return gtypes.NewResultOK(res, "")
}

func makeETHHeader(header *gtypes.Header) *etypes.Header {
	return &etypes.Header{
		ParentHash: common.BytesToHash(header.LastBlockID.Hash),
		Difficulty: big.NewInt(0),
		GasLimit:   math.MaxBig256.Uint64(),
		Time:       big.NewInt(header.Time.Unix()),
		Number:     big.NewInt(header.Height),
	}
}

func (app *EVMApp) queryNonce(addrBytes []byte) gtypes.Result {
	if len(addrBytes) != 20 {
		return gtypes.NewError(gtypes.CodeType_BaseInvalidInput, "Invalid address")
	}
	addr := common.BytesToAddress(addrBytes)

	app.stateMtx.Lock()
	nonce := app.publicState.GetNonce(addr)
	app.stateMtx.Unlock()

	data, err := rlp.EncodeToBytes(nonce)
	if err != nil {
		log.Warn("query error", zap.Error(err))
	}
	return gtypes.NewResultOK(data, "")
}

func (app *EVMApp) queryReceipt(txHashBytes []byte) gtypes.Result {
	key := append(ReceiptsPrefix, txHashBytes...)
	data, err := app.stateDb.Get(key)
	if err != nil {
		return gtypes.NewError(gtypes.CodeType_InternalError, "fail to get receipt for tx:"+string(key))
	}
	return gtypes.NewResultOK(data, "")
}

func (app *EVMApp) queryTransaction(txHashBytes []byte) gtypes.Result {
	if len(txHashBytes) == 0 {
		return gtypes.NewError(gtypes.CodeType_BaseInvalidInput, "Empty query")
	}

	var res gtypes.Result
	data, err := app.core.Query(txHashBytes[0], txHashBytes[1:])
	if err != nil {
		return gtypes.NewError(gtypes.CodeType_InternalError, err.Error())
	}

	bs, err := rlp.EncodeToBytes(data)
	if err != nil {
		return gtypes.NewError(gtypes.CodeType_InternalError, err.Error())
	}
	res.Data = bs
	res.Code = gtypes.CodeType_OK
	return res
}

func (app *EVMApp) queryPayLoad(txHashBytes []byte) gtypes.Result {
	if len(txHashBytes) == 0 {
		return gtypes.NewError(gtypes.CodeType_BaseInvalidInput, "Empty query")
	}

	var res gtypes.Result
	data, err := app.core.Query(txHashBytes[0], txHashBytes[1:])
	if err != nil {
		return gtypes.NewError(gtypes.CodeType_InternalError, err.Error())
	}

	if value, ok := data.([]byte); ok {
		res.Data = value
	}

	res.Code = gtypes.CodeType_OK
	return res
}

func (app *EVMApp) SetCore(core gtypes.Core) {
	app.core = core
}
