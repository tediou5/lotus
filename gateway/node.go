package gateway

import (
	"context"
	"fmt"
	"time"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	logger "github.com/ipfs/go-log/v2"
	"go.opencensus.io/stats"
	"golang.org/x/time/rate"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/go-bitfield"
	"github.com/filecoin-project/go-f3/certs"
	"github.com/filecoin-project/go-jsonrpc"
	"github.com/filecoin-project/go-state-types/abi"
	verifregtypes "github.com/filecoin-project/go-state-types/builtin/v9/verifreg"
	"github.com/filecoin-project/go-state-types/dline"
	"github.com/filecoin-project/go-state-types/network"

	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/build/buildconstants"
	"github.com/filecoin-project/lotus/chain/actors/builtin/miner"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/types/ethtypes"
	_ "github.com/filecoin-project/lotus/lib/sigs/bls"
	_ "github.com/filecoin-project/lotus/lib/sigs/delegated"
	_ "github.com/filecoin-project/lotus/lib/sigs/secp"
	"github.com/filecoin-project/lotus/metrics"
	"github.com/filecoin-project/lotus/node/impl/full"
	"github.com/filecoin-project/lotus/node/modules/dtypes"
)

var log = logger.Logger("gateway")

const (
	DefaultMaxLookbackDuration      = time.Hour * 24     // Default duration that a gateway request can look back in chain history
	DefaultMaxMessageLookbackEpochs = abi.ChainEpoch(20) // Default number of epochs that a gateway message lookup can look back in chain history
	DefaultRateLimitTimeout         = time.Second * 5    // Default timeout for rate limiting requests; where a request would take longer to wait than this value, it will be retjected
	DefaultEthMaxFiltersPerConn     = 16                 // Default maximum number of ETH filters and subscriptions per websocket connection

	basicRateLimitTokens  = 1
	walletRateLimitTokens = 1
	chainRateLimitTokens  = 2
	stateRateLimitTokens  = 3

	MaxRateLimitTokens = stateRateLimitTokens // Number of tokens consumed for the most expensive types of operations
)

// TargetAPI defines the API methods that the Node depends on
// (to make it easy to mock for tests)
type TargetAPI interface {
	MpoolPending(context.Context, types.TipSetKey) ([]*types.SignedMessage, error)
	ChainGetBlock(context.Context, cid.Cid) (*types.BlockHeader, error)
	MinerGetBaseInfo(context.Context, address.Address, abi.ChainEpoch, types.TipSetKey) (*api.MiningBaseInfo, error)
	GasEstimateGasPremium(context.Context, uint64, address.Address, int64, types.TipSetKey) (types.BigInt, error)
	StateReplay(context.Context, types.TipSetKey, cid.Cid) (*api.InvocResult, error)
	StateMinerSectorCount(context.Context, address.Address, types.TipSetKey) (api.MinerSectors, error)
	Version(context.Context) (api.APIVersion, error)
	ChainGetParentMessages(context.Context, cid.Cid) ([]api.Message, error)
	ChainGetParentReceipts(context.Context, cid.Cid) ([]*types.MessageReceipt, error)
	ChainGetMessagesInTipset(context.Context, types.TipSetKey) ([]api.Message, error)
	ChainGetBlockMessages(context.Context, cid.Cid) (*api.BlockMessages, error)
	ChainGetMessage(ctx context.Context, mc cid.Cid) (*types.Message, error)
	ChainGetNode(ctx context.Context, p string) (*api.IpldObject, error)
	ChainGetTipSet(ctx context.Context, tsk types.TipSetKey) (*types.TipSet, error)
	ChainGetTipSetByHeight(ctx context.Context, h abi.ChainEpoch, tsk types.TipSetKey) (*types.TipSet, error)
	ChainGetTipSetAfterHeight(ctx context.Context, h abi.ChainEpoch, tsk types.TipSetKey) (*types.TipSet, error)
	ChainHasObj(context.Context, cid.Cid) (bool, error)
	ChainHead(ctx context.Context) (*types.TipSet, error)
	ChainNotify(context.Context) (<-chan []*api.HeadChange, error)
	ChainGetPath(ctx context.Context, from, to types.TipSetKey) ([]*api.HeadChange, error)
	ChainReadObj(context.Context, cid.Cid) ([]byte, error)
	ChainPutObj(context.Context, blocks.Block) error
	ChainGetGenesis(context.Context) (*types.TipSet, error)
	GasEstimateMessageGas(ctx context.Context, msg *types.Message, spec *api.MessageSendSpec, tsk types.TipSetKey) (*types.Message, error)
	MpoolGetNonce(ctx context.Context, addr address.Address) (uint64, error)
	MpoolPushUntrusted(ctx context.Context, sm *types.SignedMessage) (cid.Cid, error)
	MsigGetAvailableBalance(ctx context.Context, addr address.Address, tsk types.TipSetKey) (types.BigInt, error)
	MsigGetVested(ctx context.Context, addr address.Address, start types.TipSetKey, end types.TipSetKey) (types.BigInt, error)
	MsigGetVestingSchedule(context.Context, address.Address, types.TipSetKey) (api.MsigVesting, error)
	MsigGetPending(ctx context.Context, addr address.Address, ts types.TipSetKey) ([]*api.MsigTransaction, error)
	StateAccountKey(ctx context.Context, addr address.Address, tsk types.TipSetKey) (address.Address, error)
	StateCall(ctx context.Context, msg *types.Message, tsk types.TipSetKey) (*api.InvocResult, error)
	StateDealProviderCollateralBounds(ctx context.Context, size abi.PaddedPieceSize, verified bool, tsk types.TipSetKey) (api.DealCollateralBounds, error)
	StateDecodeParams(ctx context.Context, toAddr address.Address, method abi.MethodNum, params []byte, tsk types.TipSetKey) (interface{}, error)
	StateGetActor(ctx context.Context, actor address.Address, ts types.TipSetKey) (*types.Actor, error)
	StateGetAllocationForPendingDeal(ctx context.Context, dealId abi.DealID, tsk types.TipSetKey) (*verifregtypes.Allocation, error)
	StateGetAllocation(ctx context.Context, clientAddr address.Address, allocationId verifregtypes.AllocationId, tsk types.TipSetKey) (*verifregtypes.Allocation, error)
	StateGetAllocations(ctx context.Context, clientAddr address.Address, tsk types.TipSetKey) (map[verifregtypes.AllocationId]verifregtypes.Allocation, error)
	StateGetClaim(ctx context.Context, providerAddr address.Address, claimId verifregtypes.ClaimId, tsk types.TipSetKey) (*verifregtypes.Claim, error)
	StateGetClaims(ctx context.Context, providerAddr address.Address, tsk types.TipSetKey) (map[verifregtypes.ClaimId]verifregtypes.Claim, error)
	StateGetNetworkParams(ctx context.Context) (*api.NetworkParams, error)
	StateLookupID(ctx context.Context, addr address.Address, tsk types.TipSetKey) (address.Address, error)
	StateListMiners(ctx context.Context, tsk types.TipSetKey) ([]address.Address, error)
	StateMarketBalance(ctx context.Context, addr address.Address, tsk types.TipSetKey) (api.MarketBalance, error)
	StateMarketStorageDeal(ctx context.Context, dealId abi.DealID, tsk types.TipSetKey) (*api.MarketDeal, error)
	StateNetworkName(context.Context) (dtypes.NetworkName, error)
	StateNetworkVersion(context.Context, types.TipSetKey) (network.Version, error)
	StateSearchMsg(ctx context.Context, from types.TipSetKey, msg cid.Cid, limit abi.ChainEpoch, allowReplaced bool) (*api.MsgLookup, error)
	StateWaitMsg(ctx context.Context, cid cid.Cid, confidence uint64, limit abi.ChainEpoch, allowReplaced bool) (*api.MsgLookup, error)
	StateReadState(ctx context.Context, actor address.Address, tsk types.TipSetKey) (*api.ActorState, error)
	StateMinerPower(context.Context, address.Address, types.TipSetKey) (*api.MinerPower, error)
	StateMinerFaults(context.Context, address.Address, types.TipSetKey) (bitfield.BitField, error)
	StateMinerRecoveries(context.Context, address.Address, types.TipSetKey) (bitfield.BitField, error)
	StateMinerInfo(context.Context, address.Address, types.TipSetKey) (api.MinerInfo, error)
	StateMinerDeadlines(context.Context, address.Address, types.TipSetKey) ([]api.Deadline, error)
	StateMinerAvailableBalance(context.Context, address.Address, types.TipSetKey) (types.BigInt, error)
	StateMinerProvingDeadline(context.Context, address.Address, types.TipSetKey) (*dline.Info, error)
	StateCirculatingSupply(context.Context, types.TipSetKey) (abi.TokenAmount, error)
	StateSectorGetInfo(ctx context.Context, maddr address.Address, n abi.SectorNumber, tsk types.TipSetKey) (*miner.SectorOnChainInfo, error)
	StateVerifiedClientStatus(ctx context.Context, addr address.Address, tsk types.TipSetKey) (*abi.StoragePower, error)
	StateVerifierStatus(ctx context.Context, addr address.Address, tsk types.TipSetKey) (*abi.StoragePower, error)
	StateVMCirculatingSupplyInternal(context.Context, types.TipSetKey) (api.CirculatingSupply, error)
	WalletBalance(context.Context, address.Address) (types.BigInt, error)

	EthAddressToFilecoinAddress(ctx context.Context, ethAddress ethtypes.EthAddress) (address.Address, error)
	FilecoinAddressToEthAddress(ctx context.Context, p jsonrpc.RawParams) (ethtypes.EthAddress, error)
	EthBlockNumber(ctx context.Context) (ethtypes.EthUint64, error)
	EthGetBlockTransactionCountByNumber(ctx context.Context, blkNum ethtypes.EthUint64) (ethtypes.EthUint64, error)
	EthGetBlockTransactionCountByHash(ctx context.Context, blkHash ethtypes.EthHash) (ethtypes.EthUint64, error)
	EthGetBlockByHash(ctx context.Context, blkHash ethtypes.EthHash, fullTxInfo bool) (ethtypes.EthBlock, error)
	EthGetBlockByNumber(ctx context.Context, blkNum string, fullTxInfo bool) (ethtypes.EthBlock, error)
	EthGetTransactionByHashLimited(ctx context.Context, txHash *ethtypes.EthHash, limit abi.ChainEpoch) (*ethtypes.EthTx, error)
	EthGetTransactionHashByCid(ctx context.Context, cid cid.Cid) (*ethtypes.EthHash, error)
	EthGetMessageCidByTransactionHash(ctx context.Context, txHash *ethtypes.EthHash) (*cid.Cid, error)
	EthGetTransactionCount(ctx context.Context, sender ethtypes.EthAddress, blkParam ethtypes.EthBlockNumberOrHash) (ethtypes.EthUint64, error)
	EthGetTransactionReceiptLimited(ctx context.Context, txHash ethtypes.EthHash, limit abi.ChainEpoch) (*api.EthTxReceipt, error)
	EthGetTransactionByBlockHashAndIndex(ctx context.Context, blkHash ethtypes.EthHash, txIndex ethtypes.EthUint64) (*ethtypes.EthTx, error)
	EthGetTransactionByBlockNumberAndIndex(ctx context.Context, blkNum string, txIndex ethtypes.EthUint64) (*ethtypes.EthTx, error)
	EthGetCode(ctx context.Context, address ethtypes.EthAddress, blkParam ethtypes.EthBlockNumberOrHash) (ethtypes.EthBytes, error)
	EthGetStorageAt(ctx context.Context, address ethtypes.EthAddress, position ethtypes.EthBytes, blkParam ethtypes.EthBlockNumberOrHash) (ethtypes.EthBytes, error)
	EthGetBalance(ctx context.Context, address ethtypes.EthAddress, blkParam ethtypes.EthBlockNumberOrHash) (ethtypes.EthBigInt, error)
	EthChainId(ctx context.Context) (ethtypes.EthUint64, error)
	EthSyncing(ctx context.Context) (ethtypes.EthSyncingResult, error)
	NetVersion(ctx context.Context) (string, error)
	NetListening(ctx context.Context) (bool, error)
	EthProtocolVersion(ctx context.Context) (ethtypes.EthUint64, error)
	EthGasPrice(ctx context.Context) (ethtypes.EthBigInt, error)
	EthFeeHistory(ctx context.Context, p jsonrpc.RawParams) (ethtypes.EthFeeHistory, error)
	EthMaxPriorityFeePerGas(ctx context.Context) (ethtypes.EthBigInt, error)
	EthEstimateGas(ctx context.Context, p jsonrpc.RawParams) (ethtypes.EthUint64, error)
	EthCall(ctx context.Context, tx ethtypes.EthCall, blkParam ethtypes.EthBlockNumberOrHash) (ethtypes.EthBytes, error)
	EthSendRawTransactionUntrusted(ctx context.Context, rawTx ethtypes.EthBytes) (ethtypes.EthHash, error)
	EthGetLogs(ctx context.Context, filter *ethtypes.EthFilterSpec) (*ethtypes.EthFilterResult, error)
	EthGetFilterChanges(ctx context.Context, id ethtypes.EthFilterID) (*ethtypes.EthFilterResult, error)
	EthGetFilterLogs(ctx context.Context, id ethtypes.EthFilterID) (*ethtypes.EthFilterResult, error)
	EthNewFilter(ctx context.Context, filter *ethtypes.EthFilterSpec) (ethtypes.EthFilterID, error)
	EthNewBlockFilter(ctx context.Context) (ethtypes.EthFilterID, error)
	EthNewPendingTransactionFilter(ctx context.Context) (ethtypes.EthFilterID, error)
	EthUninstallFilter(ctx context.Context, id ethtypes.EthFilterID) (bool, error)
	EthSubscribe(ctx context.Context, params jsonrpc.RawParams) (ethtypes.EthSubscriptionID, error)
	EthUnsubscribe(ctx context.Context, id ethtypes.EthSubscriptionID) (bool, error)
	Web3ClientVersion(ctx context.Context) (string, error)
	EthTraceBlock(ctx context.Context, blkNum string) ([]*ethtypes.EthTraceBlock, error)
	EthTraceReplayBlockTransactions(ctx context.Context, blkNum string, traceTypes []string) ([]*ethtypes.EthTraceReplayBlockTransaction, error)
	EthTraceTransaction(ctx context.Context, txHash string) ([]*ethtypes.EthTraceTransaction, error)
	EthTraceFilter(ctx context.Context, filter ethtypes.EthTraceFilterCriteria) ([]*ethtypes.EthTraceFilterResult, error)
	EthGetBlockReceiptsLimited(ctx context.Context, blkParam ethtypes.EthBlockNumberOrHash, limit abi.ChainEpoch) ([]*api.EthTxReceipt, error)
	EthGetBlockReceipts(ctx context.Context, blkParam ethtypes.EthBlockNumberOrHash) ([]*api.EthTxReceipt, error)
	GetActorEventsRaw(ctx context.Context, filter *types.ActorEventFilter) ([]*types.ActorEvent, error)
	SubscribeActorEventsRaw(ctx context.Context, filter *types.ActorEventFilter) (<-chan *types.ActorEvent, error)
	ChainGetEvents(ctx context.Context, eventsRoot cid.Cid) ([]types.Event, error)
	F3GetCertificate(ctx context.Context, instance uint64) (*certs.FinalityCertificate, error)
	F3GetLatestCertificate(ctx context.Context) (*certs.FinalityCertificate, error)
}

var _ TargetAPI = *new(api.FullNode) // gateway depends on latest

type Node struct {
	target                   TargetAPI
	subHnd                   *EthSubHandler
	maxLookbackDuration      time.Duration
	maxMessageLookbackEpochs abi.ChainEpoch
	rateLimiter              *rate.Limiter
	rateLimitTimeout         time.Duration
	ethMaxFiltersPerConn     int
	errLookback              error
}

var (
	_ api.Gateway         = (*Node)(nil)
	_ full.ChainModuleAPI = (*Node)(nil)
	_ full.GasModuleAPI   = (*Node)(nil)
	_ full.MpoolModuleAPI = (*Node)(nil)
	_ full.StateModuleAPI = (*Node)(nil)
	_ full.EthModuleAPI   = (*Node)(nil)
	_ full.EthEventAPI    = (*Node)(nil)
)

type options struct {
	subHandler               *EthSubHandler
	maxLookbackDuration      time.Duration
	maxMessageLookbackEpochs abi.ChainEpoch
	rateLimit                int
	rateLimitTimeout         time.Duration
	ethMaxFiltersPerConn     int
}

type Option func(*options)

// WithEthSubHandler sets the Ethereum subscription handler for the gateway node. This is used for
// the RPC reverse handler for EthSubscribe calls.
func WithEthSubHandler(subHandler *EthSubHandler) Option {
	return func(opts *options) {
		opts.subHandler = subHandler
	}
}

// WithMaxLookbackDuration sets the maximum lookback duration (time) for state queries.
func WithMaxLookbackDuration(maxLookbackDuration time.Duration) Option {
	return func(opts *options) {
		opts.maxLookbackDuration = maxLookbackDuration
	}
}

// WithMaxMessageLookbackEpochs sets the maximum lookback (epochs) for state queries.
func WithMaxMessageLookbackEpochs(maxMessageLookbackEpochs abi.ChainEpoch) Option {
	return func(opts *options) {
		opts.maxMessageLookbackEpochs = maxMessageLookbackEpochs
	}
}

// WithRateLimit sets the maximum number of requests per second globally that will be allowed
// before the gateway starts to rate limit requests.
func WithRateLimit(rateLimit int) Option {
	return func(opts *options) {
		opts.rateLimit = rateLimit
	}
}

// WithRateLimitTimeout sets the timeout for rate limiting requests such that when rate limiting is
// being applied, if the timeout is reached the request will be allowed.
func WithRateLimitTimeout(rateLimitTimeout time.Duration) Option {
	return func(opts *options) {
		opts.rateLimitTimeout = rateLimitTimeout
	}
}

// WithEthMaxFiltersPerConn sets the maximum number of Ethereum filters and subscriptions that can
// be maintained per websocket connection.
func WithEthMaxFiltersPerConn(ethMaxFiltersPerConn int) Option {
	return func(opts *options) {
		opts.ethMaxFiltersPerConn = ethMaxFiltersPerConn
	}
}

// NewNode creates a new gateway node.
func NewNode(api TargetAPI, opts ...Option) *Node {
	options := &options{
		maxLookbackDuration:      DefaultMaxLookbackDuration,
		maxMessageLookbackEpochs: DefaultMaxMessageLookbackEpochs,
		rateLimitTimeout:         DefaultRateLimitTimeout,
		ethMaxFiltersPerConn:     DefaultEthMaxFiltersPerConn,
	}
	for _, opt := range opts {
		opt(options)
	}

	limit := rate.Inf
	if options.rateLimit > 0 {
		limit = rate.Every(time.Second / time.Duration(options.rateLimit))
	}
	return &Node{
		target:                   api,
		subHnd:                   options.subHandler,
		maxLookbackDuration:      options.maxLookbackDuration,
		maxMessageLookbackEpochs: options.maxMessageLookbackEpochs,
		rateLimiter:              rate.NewLimiter(limit, MaxRateLimitTokens), // allow for a burst of MaxRateLimitTokens
		rateLimitTimeout:         options.rateLimitTimeout,
		errLookback:              fmt.Errorf("lookbacks of more than %s are disallowed", options.maxLookbackDuration),
		ethMaxFiltersPerConn:     options.ethMaxFiltersPerConn,
	}
}

func (gw *Node) checkTipsetKey(ctx context.Context, tsk types.TipSetKey) error {
	if tsk.IsEmpty() {
		return nil
	}

	ts, err := gw.target.ChainGetTipSet(ctx, tsk)
	if err != nil {
		return err
	}

	return gw.checkTipset(ts)
}

func (gw *Node) checkTipset(ts *types.TipSet) error {
	at := time.Unix(int64(ts.Blocks()[0].Timestamp), 0)
	if err := gw.checkTimestamp(at); err != nil {
		return fmt.Errorf("bad tipset: %w", err)
	}
	return nil
}

func (gw *Node) checkTipsetHeight(ts *types.TipSet, h abi.ChainEpoch) error {
	if h > ts.Height() {
		return fmt.Errorf("tipset height in future")
	}
	tsBlock := ts.Blocks()[0]
	heightDelta := time.Duration(uint64(tsBlock.Height-h)*buildconstants.BlockDelaySecs) * time.Second
	timeAtHeight := time.Unix(int64(tsBlock.Timestamp), 0).Add(-heightDelta)

	if err := gw.checkTimestamp(timeAtHeight); err != nil {
		return fmt.Errorf("bad tipset height: %w", err)
	}
	return nil
}

func (gw *Node) checkTimestamp(at time.Time) error {
	if time.Since(at) > gw.maxLookbackDuration {
		return gw.errLookback
	}
	return nil
}

func (gw *Node) limit(ctx context.Context, tokens int) error {
	ctx2, cancel := context.WithTimeout(ctx, gw.rateLimitTimeout)
	defer cancel()

	if perConnLimiter, ok := getPerConnectionAPIRateLimiter(ctx); ok {
		err := perConnLimiter.WaitN(ctx2, tokens)
		if err != nil {
			return fmt.Errorf("connection limited. %w", err)
		}
	}

	err := gw.rateLimiter.WaitN(ctx2, tokens)
	if err != nil {
		stats.Record(ctx, metrics.RateLimitCount.M(1))
		return fmt.Errorf("server busy. %w", err)
	}
	return nil
}
