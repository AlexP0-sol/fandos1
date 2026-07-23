// Package bitget — WebSocket subscriber for Bitget V2 public channels.
//
// VERIFIED (cross-referenced with official Bitget V2 API docs and REST ticker response):
//   - WS URL: wss://ws.bitget.com/v2/ws/public
//   - Subscribe format: {"op":"subscribe","args":[{"instType":"USDT-FUTURES","channel":"ticker","instId":"BTCUSDT"}]}
//   - Keepalive: text frame "ping" every ~20s; server responds text "pong"
//   - Push envelope: {"action":"snapshot"|"update","arg":{...},"data":[...]}
//   - Control frames: {"event":"subscribe"|"error",...} — ignore
//
// VERIFIED ticker field names (from REST /api/v2/mix/market/ticker and official WS doc):
//   - lastPr       — last traded price
//   - bidPr        — best bid price
//   - askPr        — best ask price
//   - markPrice    — mark price
//   - indexPrice   — index price
//   - fundingRate  — current funding rate (futures only)
//   - nextFundingTime — next funding epoch ms string (futures only)
//   - ts           — ticker timestamp ms string
//   - usdtVolume   — 24h USDT volume
//
// TODO:VERIFY: WS ticker may also carry bid1Price/ask1Price (UTA endpoint uses these names).
//
//	Classic/Mix channel confirmed to use bidPr/askPr per REST docs.
//
// VERIFIED orderbook channel names (from official docs):
//   - "books1"  → 1-level BBO, always snapshot
//   - "books5"  → 5-level, always snapshot
//   - "books15" → 15-level, always snapshot
//   - "books"   → full depth, snapshot then incremental
//
// VERIFIED orderbook data fields:
//   - asks  [][]string  — [price, qty]
//   - bids  [][]string
//   - ts    string      — ms timestamp
//   - seq   int64       — sequence number
package bitget

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thecd/fundarbitrage/internal/decimal"
	"github.com/thecd/fundarbitrage/internal/domain"
	exchange "github.com/thecd/fundarbitrage/internal/exchange"
)

// wsPingInterval — Bitget requests keepalive every 20–30 s; we use 20 s.
// VERIFIED: official docs say "send ping every 30 seconds"; 20 s is well within that.
const wsPingInterval = 20 * time.Second

// wsPublicBufSize — drop-on-full buffer for public events.
const wsPublicBufSize = 1024

// ============================================================
// Wire types
// ============================================================

// wsSubscribeArg — one channel arg in a subscribe request.
// VERIFIED: {"instType":"USDT-FUTURES","channel":"ticker","instId":"BTCUSDT"}
type wsSubscribeArg struct {
	InstType string `json:"instType"`
	Channel  string `json:"channel"`
	InstID   string `json:"instId"`
}

// wsSubscribeMsg — the full subscribe request.
// VERIFIED: {"op":"subscribe","args":[...]}
type wsSubscribeMsg struct {
	Op   string           `json:"op"`
	Args []wsSubscribeArg `json:"args"`
}

// wsPushMsg — inbound push frame from Bitget.
// VERIFIED: {"action":"snapshot"|"update","arg":{...},"data":[...]}
// Control frames: {"event":"subscribe"|"error","code":"...","msg":"..."}
type wsPushMsg struct {
	// Push data frame fields
	Action string          `json:"action"` // "snapshot" | "update"
	Arg    wsSubscribeArg  `json:"arg"`    // channel identity echo
	Data   json.RawMessage `json:"data"`
	// Control frame fields
	Event string `json:"event"` // "subscribe" | "error" | ""
	Code  string `json:"code"`
	Msg   string `json:"msg"`
	// Top-level timestamp (ms) in some frames
	// TODO:VERIFY: Bitget V2 WS push frames may include top-level "ts" (integer)
	Ts int64 `json:"ts"`
}

// wsTickerData — ticker data element from "ticker" channel.
// VERIFIED field names from REST /api/v2/mix/market/ticker response
// and Bitget V2 official WS docs; Mix/Classic channel uses Pr suffix.
// TODO:VERIFY: exact casing of "nextFundingTime" in WS push (matches REST verified name).
type wsTickerData struct {
	Symbol          string `json:"symbol"`
	LastPr          string `json:"lastPr"`
	BidPr           string `json:"bidPr"`
	AskPr           string `json:"askPr"`
	MarkPrice       string `json:"markPrice"`
	IndexPrice      string `json:"indexPrice"`
	FundingRate     string `json:"fundingRate"`
	NextFundingTime string `json:"nextFundingTime"`
	UsdtVolume      string `json:"usdtVolume"`
	Ts              string `json:"ts"` // ms string in data element
}

// wsBookData — orderbook data element.
// VERIFIED: from official Bitget V2 WS depth channel docs.
type wsBookData struct {
	Asks [][]string `json:"asks"` // [[price, qty], ...]
	Bids [][]string `json:"bids"`
	Ts   string     `json:"ts"`  // ms string
	Seq  int64      `json:"seq"` // VERIFIED: long int
}

// ============================================================
// SubscribePublic
// ============================================================

// SubscribePublic connects to the Bitget V2 public WebSocket and subscribes
// to the requested channels.
//
// Lifecycle mirrors bybit/ws.go:
//   - One connection per call; reconnect is the caller's responsibility.
//   - ctx cancel or read error → close channel and return.
//   - Drop-on-full with buffer wsPublicBufSize.
//   - Ping loop sends text "ping" every wsPingInterval.
func (a *Adapter) SubscribePublic(ctx context.Context, subscriptions []exchange.PublicSubscription) (<-chan exchange.PublicEvent, error) {
	args := buildBitgetSubArgs(subscriptions)
	if len(args) == 0 {
		return nil, fmt.Errorf("bitget SubscribePublic: no subscriptions")
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, a.wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("bitget SubscribePublic: dial: %w", err)
	}

	ch := make(chan exchange.PublicEvent, wsPublicBufSize)

	go func() {
		defer conn.Close()
		defer close(ch)

		// Send subscribe request.
		subMsg := wsSubscribeMsg{Op: "subscribe", Args: args}
		if err := conn.WriteJSON(subMsg); err != nil {
			return
		}

		// Ping loop: sends text "ping" (not JSON) per Bitget V2 spec.
		go wsPingLoop(ctx, conn)

		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}

			events := parseBitgetPush(raw, a)
			for _, ev := range events {
				select {
				case ch <- ev:
				default:
					// Drop on full — public events are best-effort.
				}
			}
		}
	}()

	return ch, nil
}

// buildBitgetSubArgs maps PublicSubscription slice to WS arg list.
// Deduplicates args that map to the same (channel, symbol) pair.
func buildBitgetSubArgs(subs []exchange.PublicSubscription) []wsSubscribeArg {
	seen := make(map[string]bool)
	var args []wsSubscribeArg

	for _, s := range subs {
		sym := string(s.Symbol)
		channelName := bitgetChannelName(s.Channel)
		if channelName == "" {
			continue
		}
		key := channelName + "|" + sym
		if seen[key] {
			continue
		}
		seen[key] = true
		args = append(args, wsSubscribeArg{
			InstType: productType, // "USDT-FUTURES" — VERIFIED
			Channel:  channelName,
			InstID:   sym,
		})
	}
	return args
}

// bitgetChannelName maps a domain Channel to a Bitget V2 WS channel name.
//
// VERIFIED mapping:
//   - ChannelTicker   → "ticker"   (carries lastPr/bidPr/askPr/fundingRate/nextFundingTime/markPrice)
//   - ChannelBBO      → "ticker"   (BBO extracted from ticker data)
//   - ChannelFunding  → "ticker"   (funding fields in same ticker push)
//   - ChannelDepth    → "books5"   (5-level snapshot; TODO:VERIFY if books1 preferred)
//   - ChannelMarkPrice→ "ticker"   (markPrice field in ticker)
//
// TODO:VERIFY: Bitget V2 WS also has a dedicated "mark-price" channel but ticker covers it.
func bitgetChannelName(ch exchange.Channel) string {
	switch ch {
	case exchange.ChannelTicker, exchange.ChannelBBO, exchange.ChannelFunding, exchange.ChannelMarkPrice:
		return "ticker"
	case exchange.ChannelDepth:
		return "books5" // TODO:VERIFY: use books1 for BBO-only depth; books5 for 5-level
	default:
		return ""
	}
}

// wsPingLoop sends Bitget-style text "ping" every wsPingInterval.
// VERIFIED: Bitget V2 keepalive is plain text "ping" (not JSON).
func wsPingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
				return
			}
		}
	}
}

// ============================================================
// Push frame parsing
// ============================================================

// parseBitgetPush decodes one raw WS frame and returns any domain events.
func parseBitgetPush(raw []byte, a *Adapter) []exchange.PublicEvent {
	// Fast-path: plain "pong" keepalive response.
	if string(raw) == "pong" {
		return nil
	}

	var msg wsPushMsg
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil
	}

	// Control frames (subscribe ack / error) — skip silently.
	if msg.Event != "" {
		return nil
	}

	// Only handle data push frames (action present).
	if msg.Action == "" {
		return nil
	}

	switch msg.Arg.Channel {
	case "ticker":
		return parseBitgetTickerPush(msg, a)
	case "books1", "books5", "books15", "books":
		return parseBitgetOrderbookPush(msg, a)
	}
	return nil
}

// parseBitgetTickerPush handles a ticker channel push frame.
// Emits ChannelTicker, ChannelBBO (if bid/ask present), and ChannelFunding
// (if fundingRate or nextFundingTime present).
func parseBitgetTickerPush(msg wsPushMsg, a *Adapter) []exchange.PublicEvent {
	// data is an array; take the first element.
	var items []wsTickerData
	if err := json.Unmarshal(msg.Data, &items); err != nil {
		return nil
	}
	if len(items) == 0 {
		return nil
	}

	now := a.clock()
	var events []exchange.PublicEvent

	for _, item := range items {
		sym := domain.ExchangeSymbol(item.Symbol)
		if sym == "" {
			// sym may be empty if field absent; fall back to arg instId
			sym = domain.ExchangeSymbol(msg.Arg.InstID)
		}

		// Timestamp: prefer item.Ts (data-element ts), fallback to msg.Ts.
		exTS := now
		if item.Ts != "" {
			ms, err := decimal.FromString(item.Ts)
			if err == nil {
				exTS = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		} else if msg.Ts != 0 {
			exTS = time.UnixMilli(msg.Ts).UTC()
		}

		// --- ChannelTicker ---
		lastPr, _ := parseDecimalOrZero(item.LastPr)
		markPr, _ := parseDecimalOrZero(item.MarkPrice)
		indexPr, _ := parseDecimalOrZero(item.IndexPrice)
		vol, _ := parseDecimalOrZero(item.UsdtVolume)

		ticker := &domain.Ticker{
			Symbol:         sym,
			LastPrice:      lastPr,
			MarkPrice:      markPr,
			IndexPrice:     indexPr,
			QuoteVolume24h: vol,
			Timestamp:      exTS,
		}
		events = append(events, exchange.PublicEvent{
			Channel:    exchange.ChannelTicker,
			Symbol:     sym,
			Ticker:     ticker,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})

		// --- ChannelBBO (emit if we have bid or ask) ---
		bidPr, _ := parseDecimalOrZero(item.BidPr)
		askPr, _ := parseDecimalOrZero(item.AskPr)
		if !bidPr.IsZero() || !askPr.IsZero() {
			bboSnap := &domain.OrderBookSnapshot{
				Exchange:   domain.ExchangeBitget,
				Symbol:     sym,
				Timestamp:  exTS,
				IsSnapshot: true,
			}
			if !bidPr.IsZero() {
				bboSnap.Bids = []domain.PriceLevel{{Price: bidPr}}
			}
			if !askPr.IsZero() {
				bboSnap.Asks = []domain.PriceLevel{{Price: askPr}}
			}
			events = append(events, exchange.PublicEvent{
				Channel:    exchange.ChannelBBO,
				Symbol:     sym,
				OrderBook:  bboSnap,
				ExchangeTS: exTS,
				ReceivedAt: now,
			})
		}

		// --- ChannelFunding (emit if funding fields present) ---
		if item.FundingRate != "" || item.NextFundingTime != "" {
			rate, _ := parseDecimalOrZero(item.FundingRate)
			var nextFunding time.Time
			if item.NextFundingTime != "" {
				ms, err := decimal.FromString(item.NextFundingTime)
				if err == nil {
					nextFunding = time.UnixMilli(ms.Underlying().IntPart()).UTC()
				}
			}
			untilFunding := time.Until(nextFunding)
			var confidence domain.ConfidenceLevel
			switch {
			case untilFunding < 30*time.Minute:
				confidence = domain.ConfidenceHigh
			case untilFunding < 4*time.Hour:
				confidence = domain.ConfidenceMedium
			default:
				confidence = domain.ConfidenceLow
			}
			funding := &domain.FundingInfo{
				ExchangeSymbol:       sym,
				RealizedFundingRate:  rate,
				PredictedFundingRate: rate,
				RateType:             domain.FundingRatePredicted,
				NextFundingTime:      nextFunding,
				Confidence:           confidence,
				FundingPriceType:     domain.FundingPriceMark,
			}
			events = append(events, exchange.PublicEvent{
				Channel:    exchange.ChannelFunding,
				Symbol:     sym,
				Funding:    funding,
				ExchangeTS: exTS,
				ReceivedAt: now,
			})
		}
	}

	return events
}

// parseBitgetOrderbookPush handles books1/books5/books15/books push frames.
// books1/books5/books15 always push snapshots.
// VERIFIED: snapshot action confirmed in official docs.
func parseBitgetOrderbookPush(msg wsPushMsg, a *Adapter) []exchange.PublicEvent {
	// Only process full snapshots (incremental "books" updates are TODO).
	if msg.Action != "snapshot" {
		return nil
	}

	var items []wsBookData
	if err := json.Unmarshal(msg.Data, &items); err != nil {
		return nil
	}

	now := a.clock()
	sym := domain.ExchangeSymbol(msg.Arg.InstID)
	var events []exchange.PublicEvent

	for _, item := range items {
		bids, err := parsePriceLevels(item.Bids)
		if err != nil {
			continue
		}
		asks, err := parsePriceLevels(item.Asks)
		if err != nil {
			continue
		}

		exTS := now
		if item.Ts != "" {
			ms, err := decimal.FromString(item.Ts)
			if err == nil {
				exTS = time.UnixMilli(ms.Underlying().IntPart()).UTC()
			}
		} else if msg.Ts != 0 {
			exTS = time.UnixMilli(msg.Ts).UTC()
		}

		ob := &domain.OrderBookSnapshot{
			Exchange:   domain.ExchangeBitget,
			Symbol:     sym,
			Bids:       bids,
			Asks:       asks,
			Timestamp:  exTS,
			Sequence:   item.Seq,
			IsSnapshot: true,
		}
		events = append(events, exchange.PublicEvent{
			Channel:    exchange.ChannelDepth,
			Symbol:     sym,
			OrderBook:  ob,
			ExchangeTS: exTS,
			ReceivedAt: now,
		})
	}

	return events
}
