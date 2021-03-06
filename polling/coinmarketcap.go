// Copyright (c) of parts are held by the various contributors (see the CLA)
// Licensed under the MIT License. See LICENSE file in the project root for full license information.

package polling

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/FactomWyomingEntity/prosper-pool/config"
	"github.com/cenkalti/backoff"
	"github.com/spf13/viper"
)

// CoinMarketCapDataSource is the datasource at https://coinmarketcap.com/
type CoinMarketCapDataSource struct {
	apikey string
}

func NewCoinMarketCapDataSource(conf *viper.Viper) (*CoinMarketCapDataSource, error) {
	s := new(CoinMarketCapDataSource)
	// Load api key
	s.apikey = conf.GetString(config.ConfigCoinMarketCapKey)
	if s.apikey == "" {
		return nil, fmt.Errorf("%s requires an api key", s.Name())
	}

	return s, nil
}

func (d *CoinMarketCapDataSource) Name() string {
	return "CoinMarketCap"
}

func (d *CoinMarketCapDataSource) Url() string {
	return "https://coinmarketcap.com/"
}

func (d *CoinMarketCapDataSource) ApiUrl() string {
	return "https://pro-api.coinmarketcap.com/v1/"
}

func (d *CoinMarketCapDataSource) SupportedPegs() []string {
	return CryptoAssets
}

func (d *CoinMarketCapDataSource) FetchPegPrices() (peg PegAssets, err error) {
	resp, err := d.CallCoinMarketCap()
	if err != nil {
		return nil, err
	}

	peg = make(map[string]PegItem)
	mapping := d.CurrencyIDMapping()

	// Look for each asset we support
	for _, asset := range d.SupportedPegs() {
		id := mapping[asset]
		index := fmt.Sprintf("%d", id)
		currency, ok := resp.Data[index]
		if !ok {
			continue
		}

		// Find us quote
		usdQuote, ok := currency.Quote["USD"]
		if !ok {
			continue
		}

		timestamp, err := time.Parse(d.DateFormat(), usdQuote.LastUpdated)
		if err != nil {
			// TODO: Warn?
			continue
		}

		peg[asset] = PegItem{Value: usdQuote.Price, WhenUnix: timestamp.Unix(), When: timestamp}
	}

	return
}

func (d *CoinMarketCapDataSource) FetchPegPrice(peg string) (i PegItem, err error) {
	return FetchPegPrice(peg, d.FetchPegPrices)
}

func (d *CoinMarketCapDataSource) DateFormat() string {
	// 2019-08-06T23:20:32.000Z
	// 2006-01-02T15:04:05.000Z
	return "2006-01-02T15:04:05.000Z"
}

// RecordIDMapping finds the coinmarketcap ids for each currency vs using the symbols.
func (d *CoinMarketCapDataSource) CurrencyIDMapping() map[string]int {
	return map[string]int{
		"XBT":  1,
		"ETH":  1027,
		"LTC":  2,
		"RVN":  2577,
		"XBC":  1831,
		"FCT":  1087,
		"BNB":  1839,
		"XLM":  512,
		"ADA":  2010,
		"XMR":  328,
		"DASH": 131,
		"ZEC":  1437,
		"DCR":  1168,
	}
}

func (d *CoinMarketCapDataSource) CallCoinMarketCap() (*CoinMarketCapResponse, error) {
	var resp *CoinMarketCapResponse

	operation := func() error {
		data, err := d.FetchPeggedPrices()
		if err != nil {
			return err
		}

		resp, err = d.ParseFetchedPrices(data)
		if err != nil {
			return err
		}
		return nil
	}

	err := backoff.Retry(operation, PollingExponentialBackOff())
	return resp, err
}

func (d *CoinMarketCapDataSource) ParseFetchedPrices(data []byte) (*CoinMarketCapResponse, error) {
	var resp CoinMarketCapResponse
	err := json.Unmarshal(data, &resp)
	if err != nil {
		return nil, err
	}
	return &resp, nil
}

func (d *CoinMarketCapDataSource) FetchPeggedPrices() ([]byte, error) {
	client := NewHTTPClient()
	req, err := http.NewRequest("GET", d.ApiUrl()+"cryptocurrency/quotes/latest", nil)
	if err != nil {
		return nil, err
	}

	mapping := d.CurrencyIDMapping()
	var ids []string
	for _, asset := range d.SupportedPegs() {
		ids = append(ids, fmt.Sprintf("%d", mapping[asset]))
	}

	q := url.Values{}
	q.Add("id", strings.Join(ids, ","))
	q.Add("convert", "USD") // We want usd prices

	req.Header.Set("Accepts", "application/json")
	req.Header.Add("X-CMC_PRO_API_KEY", d.apikey)
	req.URL.RawQuery = q.Encode()

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return ioutil.ReadAll(resp.Body)
}

type CoinMarketCapResponse struct {
	Status struct {
		Timestamp    string `json:"timestamp"`
		ErrorCode    int    `json:"error_code"`
		ErrorMessage string `json:"error_message"`
		Elapsed      int    `json:"elapsed"`
		CreditCount  int    `json:"credit_count"`
	} `json:"status"`

	Data map[string]CoinMarketCapCurrency `json:"data"`
}

type CoinMarketCapCurrency struct {
	ID                int       `json:"id"`
	Name              string    `json:"name"`
	Symbol            string    `json:"symbol"`
	Slug              string    `json:"slug"`
	NumMarketPairs    int       `json:"num_market_pairs"`
	DateAdded         time.Time `json:"date_added"`
	Tags              []string  `json:"tags"`
	MaxSupply         float64   `json:"max_supply"`
	CirculatingSupply float64   `json:"circulating_supply"`
	TotalSupply       float64   `json:"total_supply"`
	//Platform          string `json:"platform"`
	CmcRank     int                           `json:"cmc_rank"`
	LastUpdated string                        `json:"last_updated"`
	Quote       map[string]CoinMarketCapQuote `json:"quote"`
}

type CoinMarketCapQuote struct {
	Price            float64 `json:"price"`
	Volume24H        float64 `json:"volume_24h"`
	PercentChange1H  float64 `json:"percent_change_1h"`
	PercentChange24H float64 `json:"percent_change_24h"`
	PercentChange7D  float64 `json:"percent_change_7d"`
	MarketCap        float64 `json:"market_cap"`
	LastUpdated      string  `json:"last_updated"`
}
