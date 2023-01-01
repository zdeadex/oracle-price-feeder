package oracle

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc"

	"price-feeder/config"
	"price-feeder/oracle/client"
	"price-feeder/oracle/provider"
	"price-feeder/oracle/types"
	pfsync "price-feeder/pkg/sync"

	oracletypes "github.com/Team-Kujira/core/x/oracle/types"

	"github.com/cosmos/cosmos-sdk/telemetry"
)

// We define tickerSleep as the minimum timeout between each oracle loop. We
// define this value empirically based on enough time to collect exchange rates,
// and broadcast pre-vote and vote transactions such that they're committed in
// at least one block during each voting period.
const (
	tickerSleep = 1000 * time.Millisecond
)

// PreviousPrevote defines a structure for defining the previous prevote
// submitted on-chain.
type PreviousPrevote struct {
	ExchangeRates     string
	Salt              string
	SubmitBlockHeight int64
}

func NewPreviousPrevote() *PreviousPrevote {
	return &PreviousPrevote{
		Salt:              "",
		ExchangeRates:     "",
		SubmitBlockHeight: 0,
	}
}

// Oracle implements the core component responsible for fetching exchange rates
// for a given set of currency pairs and determining the correct exchange rates
// to submit to the on-chain price oracle adhering the oracle specification.
type Oracle struct {
	logger zerolog.Logger
	closer *pfsync.Closer

	providerTimeout    time.Duration
	providerPairs      map[provider.Name][]types.CurrencyPair
	previousPrevote    *PreviousPrevote
	previousVotePeriod float64
	priceProviders     map[provider.Name]provider.Provider
	oracleClient       client.OracleClient
	deviations         map[string]sdk.Dec
	endpoints          map[provider.Name]provider.Endpoint

	mtx             sync.RWMutex
	lastPriceSyncTS time.Time
	prices          map[string]sdk.Dec
	paramCache      ParamCache
	healthchecks    map[string]http.Client
}

func New(
	logger zerolog.Logger,
	oc client.OracleClient,
	currencyPairs []config.CurrencyPair,
	providerTimeout time.Duration,
	deviations map[string]sdk.Dec,
	endpoints map[provider.Name]provider.Endpoint,
	healthchecksConfig []config.Healthchecks,
) *Oracle {
	providerPairs := make(map[provider.Name][]types.CurrencyPair)

	for _, pair := range currencyPairs {
		for _, provider := range pair.Providers {
			providerPairs[provider] = append(providerPairs[provider], types.CurrencyPair{
				Base:  pair.Base,
				Quote: pair.Quote,
			})
		}
	}

	healthchecks := make(map[string]http.Client)
	for _, healthcheck := range healthchecksConfig {
		timeout, err := time.ParseDuration(healthcheck.Timeout)
		if err != nil {
			logger.Warn().
				Str("timeout", healthcheck.Timeout).
				Msg("failed to parse healthcheck timeout, skipping configuration")
		} else {
			healthchecks[healthcheck.URL] = http.Client{
				Timeout: timeout,
			}
		}
	}

	return &Oracle{
		logger:          logger.With().Str("module", "oracle").Logger(),
		closer:          pfsync.NewCloser(),
		oracleClient:    oc,
		providerPairs:   providerPairs,
		priceProviders:  make(map[provider.Name]provider.Provider),
		previousPrevote: nil,
		providerTimeout: providerTimeout,
		deviations:      deviations,
		paramCache:      ParamCache{},
		endpoints:       endpoints,
		healthchecks:    healthchecks,
	}
}

// Start starts the oracle process in a blocking fashion.
func (o *Oracle) Start(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			o.closer.Close()

		default:
			o.logger.Debug().Msg("starting oracle tick")

			startTime := time.Now()

			if err := o.tick(ctx); err != nil {
				telemetry.IncrCounter(1, "failure", "tick")
				o.logger.Err(err).Msg("oracle tick failed")
			}

			o.lastPriceSyncTS = time.Now()

			telemetry.MeasureSince(startTime, "runtime", "tick")
			telemetry.IncrCounter(1, "new", "tick")

			time.Sleep(tickerSleep)
		}
	}
}

// Stop stops the oracle process and waits for it to gracefully exit.
func (o *Oracle) Stop() {
	o.closer.Close()
	<-o.closer.Done()
}

// GetLastPriceSyncTimestamp returns the latest timestamp at which prices where
// fetched from the oracle's set of exchange rate providers.
func (o *Oracle) GetLastPriceSyncTimestamp() time.Time {
	o.mtx.RLock()
	defer o.mtx.RUnlock()

	return o.lastPriceSyncTS
}

// GetPrices returns a copy of the current prices fetched from the oracle's
// set of exchange rate providers.
func (o *Oracle) GetPrices() sdk.DecCoins {
	o.mtx.RLock()
	defer o.mtx.RUnlock()
	// Creates a new array for the prices in the oracle
	prices := sdk.NewDecCoins()
	for k, v := range o.prices {
		// Fills in the prices with each value in the oracle
		prices = prices.Add(sdk.NewDecCoinFromDec(k, v))
	}

	return prices
}

// SetPrices retrieves all the prices and candles from our set of providers as
// determined in the config. If candles are available, uses TVWAP in order
// to determine prices. If candles are not available, uses the most recent prices
// with VWAP. Warns the the user of any missing prices, and filters out any faulty
// providers which do not report prices or candles within 2𝜎 of the others.
func (o *Oracle) SetPrices(ctx context.Context) error {
	g := new(errgroup.Group)
	mtx := new(sync.Mutex)
	providerPrices := make(provider.AggregatedProviderPrices)
	requiredRates := make(map[string]struct{})

	for providerName, currencyPairs := range o.providerPairs {
		providerName := providerName
		currencyPairs := currencyPairs

		priceProvider, err := o.getOrSetProvider(ctx, providerName)
		if err != nil {
			return err
		}

		for _, pair := range currencyPairs {
			if _, ok := requiredRates[pair.Base]; !ok {
				requiredRates[pair.Base] = struct{}{}
			}
		}

		g.Go(func() error {
			prices := make(map[string]types.TickerPrice, 0)
			ch := make(chan struct{})
			errCh := make(chan error, 1)

			go func() {
				defer close(ch)
				prices, err = priceProvider.GetTickerPrices(currencyPairs...)
				if err != nil {
					telemetry.IncrCounter(1, "failure", "provider", "type", "ticker")
					errCh <- err
				}

			}()

			select {
			case <-ch:
				break
			case err := <-errCh:
				return err
			case <-time.After(o.providerTimeout):
				telemetry.IncrCounter(1, "failure", "provider", "type", "timeout")
				return fmt.Errorf("provider timed out: %s", providerName)
			}

			// flatten and collect prices based on the base currency per provider
			//
			// e.g.: {ProviderKraken: {"ATOM": <price, volume>, ...}}
			mtx.Lock()
			for _, pair := range currencyPairs {
				success := SetProviderTickerPrices(providerName, providerPrices, prices, pair)
				if !success {
					mtx.Unlock()
					return fmt.Errorf("failed to find any exchange rates in provider responses for '%s'", pair)
				}
			}

			mtx.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		o.logger.Debug().Err(err).Msg("failed to get ticker prices from provider")
	}

	providerPrices, err := FilterStaleTickers(
		o.logger,
		providerPrices,
		30,
	)

	if err != nil {
		return err
	}

	computedPrices, err := GetComputedPrices(
		o.logger,
		providerPrices,
		o.providerPairs,
		o.deviations,
	)
	if err != nil {
		return err
	}

	if len(computedPrices) != len(requiredRates) {
		return fmt.Errorf("unable to get prices for all exchange candles")
	}

	for base := range requiredRates {
		if _, ok := computedPrices[base]; !ok {
			return fmt.Errorf("reported prices were not equal to required rates, missed: %s", base)
		}
	}

	o.prices = computedPrices

	return nil
}

// GetComputedPrices gets the candle and ticker prices and computes it.
// It returns candles' TVWAP if possible, if not possible (not available
// or due to some staleness) it will use the most recent ticker prices
// and the VWAP formula instead.
func GetComputedPrices(
	logger zerolog.Logger,
	providerPrices provider.AggregatedProviderPrices,
	providerPairs map[provider.Name][]types.CurrencyPair,
	deviations map[string]sdk.Dec,
) (prices map[string]sdk.Dec, err error) {

	convertedTickers, err := convertTickersToUSD(
		logger,
		providerPrices,
		providerPairs,
		deviations,
	)
	if err != nil {
		return nil, err
	}

	filteredProviderPrices, err := FilterTickerDeviations(
		logger,
		convertedTickers,
		deviations,
	)
	if err != nil {
		return nil, err
	}

	vwapPrices, err := ComputeVWAP(filteredProviderPrices)
	if err != nil {
		return nil, err
	}

	return vwapPrices, nil
}

// SetProviderTickerPricesAndCandles flattens and collects prices for
// candles and tickers based on the base currency per provider.
// Returns true if at least one of price or candle exists.
func SetProviderTickerPricesAndCandles(
	providerName provider.Name,
	providerPrices provider.AggregatedProviderPrices,
	providerCandles provider.AggregatedProviderCandles,
	prices map[string]types.TickerPrice,
	candles map[string][]types.CandlePrice,
	pair types.CurrencyPair,
) (success bool) {
	if _, ok := providerPrices[providerName]; !ok {
		providerPrices[providerName] = make(map[string]types.TickerPrice)
	}
	if _, ok := providerCandles[providerName]; !ok {
		providerCandles[providerName] = make(map[string][]types.CandlePrice)
	}

	tp, pricesOk := prices[pair.String()]
	cp, candlesOk := candles[pair.String()]

	if pricesOk {
		providerPrices[providerName][pair.Base] = tp
	}
	if candlesOk {
		providerCandles[providerName][pair.Base] = cp
	}

	return pricesOk || candlesOk
}

// SetProviderTickerPrices flattens and collects prices for
// tickers based on the base currency per provider.
// Returns true if at least one of price or candle exists.
func SetProviderTickerPrices(
	providerName provider.Name,
	providerPrices provider.AggregatedProviderPrices,
	prices map[string]types.TickerPrice,
	pair types.CurrencyPair,
) (success bool) {
	if _, ok := providerPrices[providerName]; !ok {
		providerPrices[providerName] = make(map[string]types.TickerPrice)
	}

	tp, pricesOk := prices[pair.String()]

	if pricesOk {
		providerPrices[providerName][pair.Base] = tp
	}

	return pricesOk
}

// GetParamCache returns the last updated parameters of the x/oracle module
// if the current ParamCache is outdated, we will query it again.
func (o *Oracle) GetParamCache(ctx context.Context, currentBlockHeigh int64) (oracletypes.Params, error) {
	if !o.paramCache.IsOutdated(currentBlockHeigh) {
		return *o.paramCache.params, nil
	}

	params, err := o.GetParams(ctx)
	if err != nil {
		return oracletypes.Params{}, err
	}

	o.checkWhitelist(params)
	o.paramCache.Update(currentBlockHeigh, params)
	return params, nil
}

// GetParams returns the current on-chain parameters of the x/oracle module.
func (o *Oracle) GetParams(ctx context.Context) (oracletypes.Params, error) {
	grpcConn, err := grpc.Dial(
		o.oracleClient.GRPCEndpoint,
		// the Cosmos SDK doesn't support any transport security mechanism
		grpc.WithInsecure(),
		grpc.WithContextDialer(dialerFunc),
	)
	if err != nil {
		return oracletypes.Params{}, fmt.Errorf("failed to dial Cosmos gRPC service: %w", err)
	}

	defer grpcConn.Close()
	queryClient := oracletypes.NewQueryClient(grpcConn)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	queryResponse, err := queryClient.Params(ctx, &oracletypes.QueryParamsRequest{})
	if err != nil {
		return oracletypes.Params{}, fmt.Errorf("failed to get x/oracle params: %w", err)
	}

	return queryResponse.Params, nil
}

func (o *Oracle) getOrSetProvider(ctx context.Context, providerName provider.Name) (provider.Provider, error) {
	var (
		priceProvider provider.Provider
		ok            bool
	)

	priceProvider, ok = o.priceProviders[providerName]
	if !ok {
		newProvider, err := NewProvider(
			ctx,
			providerName,
			o.logger,
			o.endpoints[providerName],
			o.providerPairs[providerName]...,
		)
		if err != nil {
			return nil, err
		}
		priceProvider = newProvider

		o.priceProviders[providerName] = priceProvider
	}

	return priceProvider, nil
}

func NewProvider(
	ctx context.Context,
	providerName provider.Name,
	logger zerolog.Logger,
	endpoint provider.Endpoint,
	providerPairs ...types.CurrencyPair,
) (provider.Provider, error) {
	switch providerName {
	case provider.ProviderBinance:
		return provider.NewBinanceProvider(ctx, logger, endpoint, false, providerPairs...)

	case provider.ProviderBinanceUS:
		return provider.NewBinanceProvider(ctx, logger, endpoint, true, providerPairs...)

	case provider.ProviderKraken:
		return provider.NewKrakenProvider(ctx, logger, endpoint, providerPairs...)

	// case provider.ProviderOsmosis:
	// 	return provider.NewOsmosisProvider(endpoint), nil

	// case provider.ProviderOsmosisV2:
	// 	return provider.NewOsmosisV2Provider(ctx, logger, endpoint, providerPairs...)

	// case provider.ProviderHuobi:
	// 	return provider.NewHuobiProvider(ctx, logger, endpoint, providerPairs...)

	case provider.ProviderCoinbase:
		return provider.NewCoinbaseProvider(ctx, logger, endpoint, providerPairs...)

		// case provider.ProviderOkx:
		// 	return provider.NewOkxProvider(ctx, logger, endpoint, providerPairs...)

		// case provider.ProviderGate:
		// 	return provider.NewGateProvider(ctx, logger, endpoint, providerPairs...)

		// case provider.ProviderBitget:
		// 	return provider.NewBitgetProvider(ctx, logger, endpoint, providerPairs...)

		// case provider.ProviderMexc:
		// 	return provider.NewMexcProvider(ctx, logger, endpoint, providerPairs...)

		// case provider.ProviderCrypto:
		// 	return provider.NewCryptoProvider(ctx, logger, endpoint, providerPairs...)

		// case provider.ProviderMock:
		// 	return provider.NewMockProvider(), nil
	}

	return nil, fmt.Errorf("provider %s not found", providerName)
}

func (o *Oracle) checkWhitelist(params oracletypes.Params) {
	for _, denom := range params.Whitelist {
		symbol := strings.ToUpper(denom.Name)
		if _, ok := o.prices[symbol]; !ok {
			o.logger.Warn().Str("denom", symbol).Msg("price missing for required denom")
		}
	}
}

func (o *Oracle) tick(ctx context.Context) error {
	o.logger.Debug().Msg("executing oracle tick")

	blockHeight, err := o.oracleClient.ChainHeight.GetChainHeight()
	if err != nil {
		return err
	}
	if blockHeight < 1 {
		return fmt.Errorf("expected positive block height")
	}

	oracleParams, err := o.GetParamCache(ctx, blockHeight)
	if err != nil {
		return err
	}

	if err := o.SetPrices(ctx); err != nil {
		return err
	}

	// Get oracle vote period, next block height, current vote period, and index
	// in the vote period.
	oracleVotePeriod := int64(oracleParams.VotePeriod)
	nextBlockHeight := blockHeight + 1
	currentVotePeriod := math.Floor(float64(nextBlockHeight) / float64(oracleVotePeriod))
	indexInVotePeriod := nextBlockHeight % oracleVotePeriod

	o.logger.Info().
		Int64("vote_period", oracleVotePeriod).
		Float64("previous_vote_period", o.previousVotePeriod).
		Float64("current_vote_period", currentVotePeriod).
		Int64("indexInVotePeriod", indexInVotePeriod).
		Msg("tick")

	// Skip until new voting period. Specifically, skip when:
	// index [0, oracleVotePeriod - 1] > oracleVotePeriod - 2 OR index is 0
	if (o.previousVotePeriod != 0 && currentVotePeriod == o.previousVotePeriod) ||
		oracleVotePeriod-indexInVotePeriod < 2 || (indexInVotePeriod > 0 && indexInVotePeriod < int64(float64(oracleVotePeriod)*0.75)) {
		o.logger.Info().
			Msg("skipping until next voting period")

		return nil
	}

	// If we're past the voting period we needed to hit, reset and submit another
	// prevote.
	if o.previousVotePeriod != 0 && currentVotePeriod-o.previousVotePeriod != 1 {
		o.logger.Info().
			Msg("missing vote during voting period")
		telemetry.IncrCounter(1, "vote", "failure", "missed")

		o.previousVotePeriod = 0
		o.previousPrevote = nil
		return nil
	}

	salt, err := GenerateSalt(32)
	if err != nil {
		return err
	}

	valAddr, err := sdk.ValAddressFromBech32(o.oracleClient.ValidatorAddrString)
	if err != nil {
		return err
	}

	exchangeRatesStr := GenerateExchangeRatesString(o.GetPrices())
	hash := oracletypes.GetAggregateVoteHash(salt, exchangeRatesStr, valAddr)
	preVoteMsg := &oracletypes.MsgAggregateExchangeRatePrevote{
		Hash:      hash.String(), // hash of prices from the oracle
		Feeder:    o.oracleClient.OracleAddrString,
		Validator: valAddr.String(),
	}

	isPrevoteOnlyTx := o.previousPrevote == nil
	if isPrevoteOnlyTx {
		// This timeout could be as small as oracleVotePeriod-indexInVotePeriod,
		// but we give it some extra time just in case.
		//
		// Ref : https://github.com/terra-money/oracle-feeder/blob/baef2a4a02f57a2ffeaa207932b2e03d7fb0fb25/feeder/src/vote.ts#L222
		o.logger.Info().
			Str("hash", hash.String()).
			Str("validator", preVoteMsg.Validator).
			Str("feeder", preVoteMsg.Feeder).
			Msg("broadcasting pre-vote")
		if err := o.oracleClient.BroadcastTx(nextBlockHeight, oracleVotePeriod*2, preVoteMsg); err != nil {
			return err
		}

		currentHeight, err := o.oracleClient.ChainHeight.GetChainHeight()
		if err != nil {
			return err
		}

		o.previousVotePeriod = math.Floor(float64(currentHeight) / float64(oracleVotePeriod))
		o.previousPrevote = &PreviousPrevote{
			Salt:              salt,
			ExchangeRates:     exchangeRatesStr,
			SubmitBlockHeight: currentHeight,
		}
	} else {
		// otherwise, we're in the next voting period and thus we vote
		voteMsg := &oracletypes.MsgAggregateExchangeRateVote{
			Salt:          o.previousPrevote.Salt,
			ExchangeRates: o.previousPrevote.ExchangeRates,
			Feeder:        o.oracleClient.OracleAddrString,
			Validator:     valAddr.String(),
		}

		o.logger.Info().
			Str("exchange_rates", voteMsg.ExchangeRates).
			Str("validator", voteMsg.Validator).
			Str("feeder", voteMsg.Feeder).
			Msg("broadcasting vote")
		if err := o.oracleClient.BroadcastTx(
			nextBlockHeight,
			oracleVotePeriod-indexInVotePeriod,
			voteMsg,
		); err != nil {
			return err
		}

		o.previousPrevote = nil
		o.previousVotePeriod = 0
		o.healthchecksPing()
	}

	return nil
}

func (o *Oracle) healthchecksPing() {
	for url, client := range o.healthchecks {
		o.logger.Info().Msg("updating healthcheck status")
		_, err := client.Get(url)
		if err != nil {
			o.logger.Warn().Msg("healthcheck ping failed")
		}
	}
}

// GenerateSalt generates a random salt, size length/2,  as a HEX encoded string.
func GenerateSalt(length int) (string, error) {
	if length == 0 {
		return "", fmt.Errorf("failed to generate salt: zero length")
	}

	bytes := make([]byte, length)

	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}

	return hex.EncodeToString(bytes), nil
}

// GenerateExchangeRatesString generates a canonical string representation of
// the aggregated exchange rates.
func GenerateExchangeRatesString(prices sdk.DecCoins) string {
	prices.Sort()
	return prices.String()
}
