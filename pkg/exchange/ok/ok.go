package ok

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/viper"
	. "github.com/ztrade/trademodel"
	. "github.com/ztrade/ztrade/pkg/core"
	"github.com/ztrade/ztrade/pkg/exchange/ok/api/market"
	"github.com/ztrade/ztrade/pkg/exchange/ok/api/public"
	"github.com/ztrade/ztrade/pkg/exchange/ok/api/trade"
)

var (
	background = context.Background()

	ApiAddr       = "https://www.okx.com/"
	WSOkexPUbilc  = "wss://wsaws.okx.com:8443/ws/v5/public"
	WSOkexPrivate = "wss://wsaws.okx.com:8443/ws/v5/private"

	TypeSPOT    = "SPOT"    //币币
	TypeMARGIN  = "MARGIN"  // 币币杠杆
	TypeSWAP    = "SWAP"    //永续合约
	TypeFUTURES = "FUTURES" //交割合约
	TypeOption  = "OPTION"  //期权
)

var _ Exchange = &OkexTrader{}

func init() {
	RegisterExchange("okex", NewOkexExchange)
}

type OrderInfo struct {
	Order
	Action TradeType
	Filled bool
}

type OkexTrader struct {
	Name      string
	tradeApi  *trade.ClientWithResponses
	marketApi *market.ClientWithResponses
	publicApi *public.ClientWithResponses

	datas   chan *ExchangeData
	closeCh chan bool

	apiKey    string
	apiSecret string
	apiPwd    string
	tdMode    string

	klineLimit int
	wsUser     *WSConn
	wsPublic   *WSConn

	ordersCache     sync.Map
	stopOrdersCache sync.Map

	simpleMode bool

	watchPublics []OPParam

	prevCandleMutex sync.Mutex
	prevCandle      map[string]*Candle
	instType        string
}

func NewOkexExchange(cfg *viper.Viper, cltName string) (e Exchange, err error) {
	b, err := NewOkexTrader(cfg, cltName)
	if err != nil {
		return
	}
	e = b
	return
}

func NewOkexTrader(cfg *viper.Viper, cltName string) (b *OkexTrader, err error) {
	b = new(OkexTrader)
	b.Name = "okex"
	b.instType = "SWAP"
	b.prevCandle = make(map[string]*Candle)
	b.simpleMode = true
	if cltName == "" {
		cltName = "okex"
	}
	b.klineLimit = 100
	// isDebug := cfg.GetBool(fmt.Sprintf("exchanges.%s.debug", cltName))
	b.apiKey = cfg.GetString(fmt.Sprintf("exchanges.%s.key", cltName))
	b.apiSecret = cfg.GetString(fmt.Sprintf("exchanges.%s.secret", cltName))
	b.apiPwd = cfg.GetString(fmt.Sprintf("exchanges.%s.pwd", cltName))
	b.tdMode = cfg.GetString(fmt.Sprintf("exchanges.%s.tdmode", cltName))
	if b.tdMode == "" {
		b.tdMode = "isolated"
	}
	simpleMode := cfg.GetString(fmt.Sprintf("exchanges.%s.simple", cltName))
	if simpleMode == "false" {
		b.simpleMode = false
	}
	log.Infof("okex %s simpleMode %t, tdMode: %s:", cltName, b.simpleMode, b.tdMode)

	b.datas = make(chan *ExchangeData, 1024)
	b.closeCh = make(chan bool)

	b.tradeApi, err = trade.NewClientWithResponses(ApiAddr)
	if err != nil {
		return
	}
	b.marketApi, err = market.NewClientWithResponses(ApiAddr)
	if err != nil {
		return
	}
	b.publicApi, err = public.NewClientWithResponses(ApiAddr)
	if err != nil {
		return
	}
	clientProxy := cfg.GetString("proxy")
	if clientProxy != "" {
		var proxyURL *url.URL
		proxyURL, err = url.Parse(clientProxy)
		if err != nil {
			return
		}
		clt := b.marketApi.ClientInterface.(*market.Client).Client.(*http.Client)
		*clt = http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
		clt = b.tradeApi.ClientInterface.(*trade.Client).Client.(*http.Client)
		*clt = http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
		websocket.DefaultDialer.Proxy = http.ProxyURL(proxyURL)
		websocket.DefaultDialer.HandshakeTimeout = time.Second * 60
	}
	b.Start(map[string]interface{}{})
	return
}

func (b *OkexTrader) SetInstType(instType string) {
	b.instType = instType
}

func (b *OkexTrader) auth(ctx context.Context, req *http.Request) (err error) {
	var temp []byte
	if req.Method != "GET" {
		temp, err = ioutil.ReadAll(req.Body)
		if err != nil {
			return
		}
		req.Body.Close()
		buf := bytes.NewBuffer(temp)
		req.Body = io.NopCloser(buf)
	} else {
		temp = []byte(fmt.Sprintf("?%s", req.URL.RawQuery))
	}
	var signStr string
	tmStr := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	signStr = fmt.Sprintf("%s%s%s%s", tmStr, req.Method, req.URL.Path, string(temp))

	h := hmac.New(sha256.New, []byte(b.apiSecret))
	h.Write([]byte(signStr))
	ret := h.Sum(nil)
	n := base64.StdEncoding.EncodedLen(len(ret))
	dst := make([]byte, n)
	base64.StdEncoding.Encode(dst, ret)
	sign := string(dst)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OK-ACCESS-KEY", b.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", sign)

	req.Header.Set("OK-ACCESS-TIMESTAMP", tmStr)
	req.Header.Set("OK-ACCESS-PASSPHRASE", b.apiPwd)
	return
}

func (b *OkexTrader) Start(param map[string]interface{}) (err error) {
	err = b.runPublic()
	if err != nil {
		return
	}
	err = b.runPrivate()
	if err != nil {
		return
	}
	return
}
func (b *OkexTrader) Stop() (err error) {
	b.wsPublic.Close()
	b.wsUser.Close()
	close(b.closeCh)
	close(b.datas)
	return
}

// KlineChan get klines
func (b *OkexTrader) GetKline(symbol, bSize string, start, end time.Time) (data chan *Candle, errCh chan error) {
	data = make(chan *Candle, 1024*10)
	errCh = make(chan error, 1)
	go func() {
		defer func() {
			close(data)
			close(errCh)
		}()
		nStart := start.Unix() * 1000
		nEnd := end.Unix() * 1000
		tempEnd := nEnd
		var nPrevStart int64
		var resp *market.GetApiV5MarketHistoryCandlesResponse
		var startStr, endStr string
		var err error
		for {
			ctx, cancel := context.WithTimeout(background, time.Second*3)
			startStr = strconv.FormatInt(nStart, 10)
			tempEnd = nStart + 100*60*1000
			endStr = strconv.FormatInt(tempEnd, 10)
			var params = market.GetApiV5MarketHistoryCandlesParams{InstId: symbol, Bar: &bSize, Before: &startStr, After: &endStr}
			resp, err = b.marketApi.GetApiV5MarketHistoryCandlesWithResponse(ctx, &params)
			cancel()
			if err != nil {
				errCh <- err
				return
			}
			klines, err := parseCandles(resp)
			if err != nil {
				if strings.Contains(err.Error(), "Requests too frequent.") {
					time.Sleep(time.Second)
					continue
				}
				errCh <- err
				return
			}
			sort.Slice(klines, func(i, j int) bool {
				return klines[i].Start < klines[j].Start
			})

			for _, v := range klines {
				if v.Start*1000 <= nPrevStart {
					continue
				}
				data <- v
				nStart = v.Start * 1000
			}
			if len(klines) == 0 {
				nStart = tempEnd
			}
			if nStart >= nEnd || nStart <= nPrevStart {
				fmt.Println(time.Unix(nStart/1000, 0), start, end)
				break
			}
			nPrevStart = nStart
		}
	}()

	return
}

func (b *OkexTrader) Watch(param WatchParam) (err error) {
	symbol := param.Extra.(string)
	log.Info("okex watch:", param)
	switch param.Type {
	case EventWatchCandle:
		var p = OPParam{
			OP: "subscribe",
			Args: []interface{}{
				OPArg{Channel: "candle1m", InstType: b.instType, InstID: symbol},
			},
		}
		b.watchPublics = append(b.watchPublics, p)
		err = b.wsPublic.WriteMsg(p)
	case EventDepth:
		var p = OPParam{
			OP: "subscribe",
			Args: []interface{}{
				OPArg{Channel: "books5", InstType: b.instType, InstID: symbol},
			},
		}
		b.watchPublics = append(b.watchPublics, p)
		err = b.wsPublic.WriteMsg(p)
	case EventTradeMarket:
		var p = OPParam{
			OP: "subscribe",
			Args: []interface{}{
				OPArg{Channel: "trades", InstType: b.instType, InstID: symbol},
			},
		}
		b.watchPublics = append(b.watchPublics, p)
		err = b.wsPublic.WriteMsg(p)
	default:
		err = fmt.Errorf("unknown wath param: %s", param.Type)
	}
	return
}
func (b *OkexTrader) processStopOrder(act TradeAction) (ret *Order, err error) {
	ctx, cancel := context.WithTimeout(background, time.Second*2)
	defer cancel()
	var side, posSide string
	// open: side = posSide, close: side!=posSide
	if act.Action.IsLong() {
		side = "buy"
		posSide = "short"
	} else {
		side = "sell"
		posSide = "long"
	}
	reduceOnly := true
	var orderPx = "-1"
	triggerPx := fmt.Sprintf("%f", act.Price)
	// PostApiV5TradeOrderAlgoJSONBody defines parameters for PostApiV5TradeOrderAlgo.
	params := trade.PostApiV5TradeOrderAlgoJSONBody{
		// 非必填<br>保证金币种，如：USDT<br>仅适用于单币种保证金模式下的全仓杠杆订单
		//	Ccy *string `json:"ccy,omitempty"`

		// 必填<br>产品ID，如：`BTC-USDT`
		InstId: act.Symbol,

		// 必填<br>订单类型。<br>`conditional`：单向止盈止损<br>`oco`：双向止盈止损<br>`trigger`：计划委托<br>`iceberg`：冰山委托<br>`twap`：时间加权委托
		OrdType: "conditional",

		// 非必填<br>委托价格<br>委托价格为-1时，执行市价委托<br>适用于`计划委托`
		OrderPx: &orderPx,

		// 可选<br>持仓方向<br>在双向持仓模式下必填，且仅可选择 `long` 或 `short`
		PosSide: &posSide,

		// 非必填<br>挂单限制价<br>适用于`冰山委托`和`时间加权委托`
		//	PxLimit *string `json:"pxLimit,omitempty"`

		// 非必填<br>距离盘口的比例价距<br>适用于`冰山委托`和`时间加权委托`
		//	PxSpread *string `json:"pxSpread,omitempty"`

		// 非必填<br>距离盘口的比例<br>pxVar和pxSpread只能传入一个<br>适用于`冰山委托`和`时间加权委托`
		//	PxVar *string `json:"pxVar,omitempty"`

		// 非必填<br>是否只减仓，`true` 或 `false`，默认`false`<br>仅适用于币币杠杆订单
		ReduceOnly: &reduceOnly,

		// 必填<br>订单方向。买：`buy` 卖：`sell`
		Side: side,

		// 非必填<br>止损委托价，如果填写此参数，必须填写止损触发价<br>委托价格为-1时，执行市价止损<br>适用于`止盈止损委托`
		SlOrdPx: &orderPx,

		// 非必填<br>止损触发价，如果填写此参数，必须填写止损委托价<br>适用于`止盈止损委托`
		SlTriggerPx: &triggerPx,

		// 必填<br>委托数量
		Sz: fmt.Sprintf("%d", int(act.Amount)),

		// 非必填<br>单笔数量<br>适用于`冰山委托`和`时间加权委托`
		//	SzLimit *string `json:"szLimit,omitempty"`

		// 必填<br>交易模式<br>保证金模式：`isolated`：逐仓 ；`cross`<br>全仓非保证金模式：`cash`：非保证金
		TdMode: b.tdMode,

		// 非必填<br>市价单委托数量的类型<br>交易货币：`base_ccy`<br>计价货币：`quote_ccy`<br>仅适用于币币订单
		//	TgtCcy *string `json:"tgtCcy,omitempty"`

		// 非必填<br>挂单限制价<br>适用于`时间加权委托`
		//	TimeInterval *string `json:"timeInterval,omitempty"`

		// 非必填<br>止盈委托价，如果填写此参数，必须填写止盈触发价<br>委托价格为-1时，执行市价止盈<br>适用于`止盈止损委托`
		//        TpOrdPx ,

		// 非必填<br>止盈触发价，如果填写此参数，必须填写止盈委托价<br>适用于`止盈止损委托`
		//	TpTriggerPx *string `json:"tpTriggerPx,omitempty"`

		// 非必填<br>计划委托触发价格<br>适用于`计划委托`
		//	TriggerPx *string `json:"triggerPx,omitempty"`
	}
	if b.simpleMode {
		params.PosSide = nil
	}
	resp, err := b.tradeApi.PostApiV5TradeOrderAlgoWithResponse(ctx, params, b.auth)
	if err != nil {
		return
	}

	orders, err := parsePostAlgoOrders(act.Symbol, "open", side, act.Price, act.Amount, resp.Body)
	if err != nil {
		return
	}
	if len(orders) != 1 {
		err = fmt.Errorf("orders len not match: %#v", orders)
		log.Warnf(err.Error())
		return
	}
	ret = orders[0]
	ret.Remark = "stop"
	return
}
func (b *OkexTrader) CancelOrder(old *Order) (order *Order, err error) {
	_, ok := b.ordersCache.Load(old.OrderID)
	if ok {
		order, err = b.cancelNormalOrder(old)
		if err != nil {
			return
		}
		b.ordersCache.Delete(old.OrderID)
	}
	_, ok = b.stopOrdersCache.Load(old.OrderID)
	if ok {
		order, err = b.cancelAlgoOrder(old)
		b.stopOrdersCache.Delete(old.OrderID)
	}
	return
}

func (b *OkexTrader) cancelNormalOrder(old *Order) (order *Order, err error) {
	ctx, cancel := context.WithTimeout(background, time.Second*2)
	defer cancel()

	var body trade.PostApiV5TradeCancelOrderJSONRequestBody
	body.InstId = old.Symbol
	body.OrdId = &old.OrderID

	cancelResp, err := b.tradeApi.PostApiV5TradeCancelOrderWithResponse(ctx, body, b.auth)
	if err != nil {
		return
	}
	temp := OKEXOrder{}
	err = json.Unmarshal(cancelResp.Body, &temp)
	if err != nil {
		return
	}
	if temp.Code != "0" {
		err = errors.New(string(cancelResp.Body))
	}
	order = old
	if len(temp.Data) > 0 {
		order.OrderID = temp.Data[0].OrdID
	}
	return
}

func (b *OkexTrader) cancelAlgoOrder(old *Order) (order *Order, err error) {
	ctx, cancel := context.WithTimeout(background, time.Second*2)
	defer cancel()

	var body = make(trade.PostApiV5TradeCancelAlgosJSONBody, 1)
	body[0] = trade.CancelAlgoOrder{AlgoId: old.OrderID, InstId: old.Symbol}

	cancelResp, err := b.tradeApi.PostApiV5TradeCancelAlgosWithResponse(ctx, body, b.auth)
	if err != nil {
		return
	}
	temp := OKEXAlgoOrder{}
	err = json.Unmarshal(cancelResp.Body, &temp)
	if err != nil {
		return
	}
	if temp.Code != "0" {
		err = errors.New(string(cancelResp.Body))
	}
	order = old
	if len(temp.Data) > 0 {
		order.OrderID = temp.Data[0].AlgoID
	}
	return
}

func (b *OkexTrader) ProcessOrder(act TradeAction) (ret *Order, err error) {
	if act.Action.IsStop() {
		ret, err = b.processStopOrder(act)
		if err != nil {
			return
		}
		b.stopOrdersCache.Store(ret.OrderID, ret)
		return
	}
	ctx, cancel := context.WithTimeout(background, time.Second*2)
	defer cancel()
	var side, posSide, px string
	if act.Action.IsLong() {
		side = "buy"
		if act.Action.IsOpen() {
			posSide = "long"
		} else {
			posSide = "short"
		}
	} else {
		side = "sell"
		if act.Action.IsOpen() {
			posSide = "short"
		} else {
			posSide = "long"
		}
	}
	ordType := "limit"
	tag := "ztrade"
	px = fmt.Sprintf("%f", act.Price)
	params := trade.PostApiV5TradeOrderJSONRequestBody{
		//ClOrdId *string `json:"clOrdId,omitempty"`
		// 必填<br>产品ID，如：`BTC-USDT`
		InstId: act.Symbol,
		// 必填<br>订单类型。<br>市价单：`market`<br>限价单：`limit`<br>只做maker单：`post_only`<br>全部成交或立即取消：`fok`<br>立即成交并取消剩余：`ioc`<br>市价委托立即成交并取消剩余：`optimal_limit_ioc`（仅适用交割、永续）
		OrdType: ordType,

		// 可选<br>持仓方向<br>在双向持仓模式下必填，且仅可选择 `long` 或 `short`
		PosSide: &posSide,

		// 可选<br>委托价格<br>仅适用于`limit`、`post_only`、`fok`、`ioc`类型的订单
		Px: &px,

		// 非必填<br>是否只减仓，`true` 或 `false`，默认`false`<br>仅适用于币币杠杆订单
		//		ReduceOnly: &reduceOnly,
		// 必填<br>订单方向。买：`buy` 卖：`sell`
		Side: side,
		// 必填<br>委托数量
		Sz: fmt.Sprintf("%d", int(act.Amount)),
		// 非必填<br>订单标签<br>字母（区分大小写）与数字的组合，可以是纯字母、纯数字，且长度在1-8位之间。
		Tag: &tag,
		// 必填<br>交易模式<br>保证金模式：`isolated`：逐仓 ；`cross`<br>全仓非保证金模式：`cash`：非保证金
		TdMode: b.tdMode,
		// 非必填<br>市价单委托数量的类型<br>交易货币：`base_ccy`<br>计价货币：`quote_ccy`<br>仅适用于币币订单
		//	TgtCcy *string `json:"tgtCcy,omitempty"`
	}
	if b.simpleMode {
		params.PosSide = nil
	}
	resp, err := b.tradeApi.PostApiV5TradeOrderWithResponse(ctx, params, b.auth)
	if err != nil {
		return
	}
	orders, err := parsePostOrders(act.Symbol, "open", side, act.Price, act.Amount, resp.Body)
	if err != nil {
		return
	}
	if len(orders) != 1 {
		err = fmt.Errorf("orders len not match: %#v", orders)
		log.Warnf(err.Error())
		return
	}
	ret = orders[0]
	b.ordersCache.Store(ret.OrderID, ret)
	return
}

type CancelNormalResp struct {
	Code string        `json:"code"`
	Msg  string        `json:"msg"`
	Data []OrderNormal `json:"data"`
}

type CancelAlgoResp struct {
	Code string      `json:"code"`
	Msg  string      `json:"msg"`
	Data []AlgoOrder `json:"data"`
}

func (b *OkexTrader) cancelAllNormal() (orders []*Order, err error) {
	ctx, cancel := context.WithTimeout(background, time.Second*3)
	defer cancel()
	instType := b.instType
	var params = trade.GetApiV5TradeOrdersPendingParams{
		// InstId:   &b.symbol,
		InstType: &instType,
	}
	resp, err := b.tradeApi.GetApiV5TradeOrdersPendingWithResponse(ctx, &params, b.auth)
	if err != nil {
		return
	}
	var orderResp CancelNormalResp
	err = json.Unmarshal(resp.Body, &orderResp)
	if err != nil {
		return
	}
	if orderResp.Code != "0" {
		err = errors.New(string(resp.Body))
		return
	}
	if len(orderResp.Data) == 0 {
		return
	}

	var body trade.PostApiV5TradeCancelBatchOrdersJSONRequestBody
	for _, v := range orderResp.Data {
		temp := v.OrdID
		body = append(body, trade.CancelBatchOrder{
			InstId: v.InstID,
			OrdId:  &temp,
		})
	}

	cancelResp, err := b.tradeApi.PostApiV5TradeCancelBatchOrdersWithResponse(ctx, body, b.auth)
	if err != nil {
		return
	}
	temp := OKEXOrder{}
	err = json.Unmarshal(cancelResp.Body, &temp)
	if err != nil {
		return
	}
	if temp.Code != "0" {
		err = errors.New(string(cancelResp.Body))
	}
	return
}

func (b *OkexTrader) cancelAllAlgo() (orders []*Order, err error) {
	ctx, cancel := context.WithTimeout(background, time.Second*3)
	defer cancel()
	instType := b.instType
	var params = trade.GetApiV5TradeOrdersAlgoPendingParams{
		OrdType: "conditional",
		// InstId:   &b.symbol,
		InstType: &instType,
	}
	resp, err := b.tradeApi.GetApiV5TradeOrdersAlgoPendingWithResponse(ctx, &params, b.auth)
	if err != nil {
		return
	}
	var orderResp CancelAlgoResp
	err = json.Unmarshal(resp.Body, &orderResp)
	if err != nil {
		return
	}
	if orderResp.Code != "0" {
		err = errors.New(string(resp.Body))
		return
	}
	if len(orderResp.Data) == 0 {
		return
	}

	var body trade.PostApiV5TradeCancelAlgosJSONRequestBody
	for _, v := range orderResp.Data {
		body = append(body, trade.CancelAlgoOrder{
			InstId: v.InstID,
			AlgoId: v.AlgoID,
		})
	}

	cancelResp, err := b.tradeApi.PostApiV5TradeCancelAlgosWithResponse(ctx, body, b.auth)
	if err != nil {
		return
	}
	temp := OKEXAlgoOrder{}
	err = json.Unmarshal(cancelResp.Body, &temp)
	if err != nil {
		return
	}
	if temp.Code != "0" {
		err = errors.New(string(cancelResp.Body))
	}
	return
}
func (b *OkexTrader) CancelAllOrders() (orders []*Order, err error) {
	temp, err := b.cancelAllNormal()
	if err != nil {
		return
	}
	orders, err = b.cancelAllAlgo()
	if err != nil {
		return
	}
	orders = append(temp, orders...)
	return
}

func (b *OkexTrader) GetSymbols() (symbols []SymbolInfo, err error) {
	ctx, cancel := context.WithTimeout(background, time.Second*3)
	defer cancel()
	resp, err := b.publicApi.GetApiV5PublicInstrumentsWithResponse(ctx, &public.GetApiV5PublicInstrumentsParams{InstType: b.instType})
	if err != nil {
		return
	}
	var instruments InstrumentResp
	err = json.Unmarshal(resp.Body, &instruments)
	if instruments.Code != "0" {
		err = errors.New(string(resp.Body))
		return
	}
	var value float64
	symbols = make([]SymbolInfo, len(instruments.Data))
	for i, v := range instruments.Data {
		value, err = strconv.ParseFloat(v.TickSz, 64)
		if err != nil {
			return
		}
		symbols[i] = SymbolInfo{
			Exchange:    "okx",
			Symbol:      v.InstID,
			Resolutions: "1m,5m,15m,30m,1h,4h,1d,1w",
			Pricescale:  int(float64(1) / value),
		}
	}
	return
}

func (b *OkexTrader) GetDataChan() chan *ExchangeData {
	return b.datas
}

func transCandle(values [7]string) (ret *Candle) {
	nTs, err := strconv.ParseInt(values[0], 10, 64)
	if err != nil {
		panic(fmt.Sprintf("trans candle error: %#v", values))
		return nil
	}
	ret = &Candle{
		ID:       0,
		Start:    nTs / 1000,
		Open:     parseFloat(values[1]),
		High:     parseFloat(values[2]),
		Low:      parseFloat(values[3]),
		Close:    parseFloat(values[4]),
		Volume:   parseFloat(values[5]),
		Turnover: parseFloat(values[6]),
	}
	return
}

func parseFloat(str string) float64 {
	if str == "" {
		return 0
	}
	f, err := strconv.ParseFloat(str, 64)
	if err != nil {
		panic("okex parseFloat error:" + err.Error())
	}
	return f
}

func parseCandles(resp *market.GetApiV5MarketHistoryCandlesResponse) (candles []*Candle, err error) {
	var candleResp CandleResp
	err = json.Unmarshal(resp.Body, &candleResp)
	if err != nil {
		return
	}
	if candleResp.Code != "0" {
		err = errors.New(string(resp.Body))
		return
	}
	for _, v := range candleResp.Data {
		temp := transCandle(v)
		candles = append(candles, temp)
	}
	return
}

func parsePostOrders(symbol, status, side string, amount, price float64, body []byte) (ret []*Order, err error) {
	temp := OKEXOrder{}
	err = json.Unmarshal(body, &temp)
	if err != nil {
		return
	}
	if temp.Code != "0" {
		err = fmt.Errorf("error resp: %s", string(body))
		return
	}
	for _, v := range temp.Data {
		if v.SCode != "0" {
			err = fmt.Errorf("%s %s", v.SCode, v.SMsg)
			return
		}

		temp := &Order{
			OrderID: v.OrdID,
			Symbol:  symbol,
			// Currency
			Side:   side,
			Status: status,
			Price:  price,
			Amount: amount,
			Time:   time.Now(),
		}
		ret = append(ret, temp)
	}
	return
}

func parsePostAlgoOrders(symbol, status, side string, amount, price float64, body []byte) (ret []*Order, err error) {
	temp := OKEXAlgoOrder{}
	err = json.Unmarshal(body, &temp)
	if err != nil {
		return
	}
	if temp.Code != "0" {
		err = fmt.Errorf("error resp: %s", string(body))
		return
	}
	for _, v := range temp.Data {
		if v.SCode != "0" {
			err = fmt.Errorf("%s %s", v.SCode, v.SMsg)
			return
		}

		temp := &Order{
			OrderID: v.AlgoID,
			Symbol:  symbol,
			// Currency
			Side:   side,
			Status: status,
			Price:  price,
			Amount: amount,
			Time:   time.Now(),
		}
		ret = append(ret, temp)
	}
	return
}

type CandleResp struct {
	Code string      `json:"code"`
	Msg  string      `json:"msg"`
	Data [][7]string `json:"data"`
}

type OKEXOrder struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		ClOrdID string `json:"clOrdId"`
		OrdID   string `json:"ordId"`
		Tag     string `json:"tag"`
		SCode   string `json:"sCode"`
		SMsg    string `json:"sMsg"`
	} `json:"data"`
}

type OKEXAlgoOrder struct {
	Code string `json:"code"`
	Msg  string `json:"msg"`
	Data []struct {
		AlgoID string `json:"algoId"`
		SCode  string `json:"sCode"`
		SMsg   string `json:"sMsg"`
	} `json:"data"`
}

type InstrumentResp struct {
	Code string       `json:"code"`
	Msg  string       `json:"msg"`
	Data []Instrument `json:"data"`
}

type Instrument struct {
	InstType  string `json:"instType"`  // 产品类型
	InstID    string `json:"instId"`    // 产品id， 如 BTC-USD-SWAP
	Uly       string `json:"uly"`       // 标的指数，如 BTC-USD，仅适用于交割/永续/期权
	Category  string `json:"category"`  // 手续费档位，每个交易产品属于哪个档位手续费
	BaseCcy   string `json:"baseCcy"`   // 交易货币币种，如 BTC-USDT 中的 BTC ，仅适用于币币
	QuoteCcy  string `json:"quoteCcy"`  // 计价货币币种，如 BTC-USDT 中的USDT ，仅适用于币币
	SettleCcy string `json:"settleCcy"` // 盈亏结算和保证金币种，如 BTC 仅适用于交割/永续/期权
	CtVal     string `json:"ctVal"`     // 合约面值，仅适用于交割/永续/期权
	CtMult    string `json:"ctMult"`    // 合约乘数，仅适用于交割/永续/期权
	CtValCcy  string `json:"ctValCcy"`  // 合约面值计价币种，仅适用于交割/永续/期权
	OptType   string `json:"optType"`   // 期权类型，C或P 仅适用于期权
	Stk       string `json:"stk"`       // 行权价格，仅适用于期权
	ListTime  string `json:"listTime"`  // 上线日期 Unix时间戳的毫秒数格式，如 1597026383085
	ExpTime   string `json:"expTime"`   // 交割/行权日期，仅适用于交割 和 期权 Unix时间戳的毫秒数格式，如 1597026383085
	Lever     string `json:"lever"`     // 该instId支持的最大杠杆倍数，不适用于币币、期权
	TickSz    string `json:"tickSz"`    // 下单价格精度，如 0.0001
	LotSz     string `json:"lotSz"`     // 下单数量精度，如 BTC-USDT-SWAP：1
	MinSz     string `json:"minSz"`     // 最小下单数量
	CtType    string `json:"ctType"`    // linear：正向合约 inverse：反向合约 仅适用于交割/永续
	Alias     string `json:"alias"`     // 合约日期别名 this_week：本周 next_week：次周 quarter：季度 next_quarter：次季度 仅适用于交割
	State     string `json:"state"`     // 产品状态 live：交易中 suspend：暂停中 preopen：预上线settlement：资金费结算
}
