package stratum

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/FactomWyomingEntity/prosper-pool/mining"
	log "github.com/sirupsen/logrus"
)

var _ = log.Println

// Clients talk to stratum servers. They are on the miner side of things, so their config's
// should be extremely light, if any.
type Client struct {
	enc  *json.Encoder
	dec  *bufio.Reader
	conn net.Conn

	version        string
	username       string
	minername      string
	currentJobID   string
	currentOPRHash string
	currentTarget  uint64

	password      string // only needed for initial user registration
	invitecode    string // only needed for initial user registration
	payoutaddress string // only needed for initial user registration

	miners         []*ControlledMiner
	successes      chan *mining.Winner
	totalSuccesses uint64 // Total submitted shares

	subscriptions []Subscription
	requestsMade  map[int32]func(Response)
	autoreconnect bool
	sync.RWMutex
}

type ControlledMiner struct {
	Miner          *mining.PegnetMiner
	CommandChannel chan *mining.MinerCommand
}

func (c *ControlledMiner) SendCommand(command *mining.MinerCommand) bool {
	select {
	case c.CommandChannel <- command:
		return true
	default:
		return false
	}
}

func NewClient(username, minername, password, invitecode, payoutaddress, version string) (*Client, error) {
	c := new(Client)
	c.autoreconnect = true
	c.version = version
	c.username = username
	c.minername = minername
	c.password = password
	c.invitecode = invitecode
	c.payoutaddress = payoutaddress
	c.currentJobID = "1"
	c.currentOPRHash = "00037f39cf870a1f49129f9c82d935665d352ffd25ea3296208f6f7b16fd654f"
	c.currentTarget = 0xfffe000000000000
	c.requestsMade = make(map[int32]func(Response))

	successChannel := make(chan *mining.Winner, 100)
	c.successes = successChannel

	go c.ListenForSuccess()
	//
	//c.miner = mining.NewPegnetMiner(1, commandChannel, successChannel)
	//go c.miner.Mine(context.Background())
	return c, nil
}

func (c *Client) InitMiners(num int) {
	c.miners = make([]*ControlledMiner, num)
	for i := range c.miners {
		commandChannel := make(chan *mining.MinerCommand, 15)
		c.miners[i] = &ControlledMiner{
			CommandChannel: commandChannel,
			Miner:          mining.NewPegnetMiner(uint32(i), commandChannel, c.successes),
		}
	}
}

func (c *Client) SetFakeHashRate(rate int) {
	for i := range c.miners {
		c.miners[i].Miner.SetFakeHashRate(rate)
	}
}

func (c *Client) SendCommand(command *mining.MinerCommand) {
	for i := range c.miners {
		c.miners[i].SendCommand(command)
	}
}

func (c *Client) RunMiners(ctx context.Context) {
	for i := range c.miners {
		go c.miners[i].Miner.Mine(ctx)
	}
}

func (c *Client) Encode(x interface{}) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("Connection issues (possibly dropped)\n")
		}
	}()
	c.Lock()
	defer c.Unlock()

	err = c.enc.Encode(x)
	return
}

func (c *Client) Connect(address string) error {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return err
	}

	conn, err := net.DialTCP("tcp", nil, addr)
	c.InitConn(conn)
	return err
}

func (c *Client) Handshake() error {
	err := c.Subscribe()
	if err != nil {
		return err
	}

	return c.Authorize(fmt.Sprintf("%s,%s", c.username, c.minername), c.password, c.invitecode, c.payoutaddress)
}

// InitConn will not start the handshake process. Good for unit tests
func (c *Client) InitConn(conn net.Conn) {
	c.Lock()
	defer c.Unlock()
	c.conn = conn
	c.enc = json.NewEncoder(conn)
	c.dec = bufio.NewReader(conn)
}

func (c *Client) BlockTillConnected(address, waittime string) error {
	for c.autoreconnect {
		if err := c.WaitThenConnect(address, waittime); err == nil {
			return nil
		} else {
			log.WithError(err).Errorf("failed to reconnect, will try again in %ss", waittime)
		}
	}
	return fmt.Errorf("miner no longer reconnecting")
}

// Wait waittime seconds, then proceed with Connect
func (c *Client) WaitThenConnect(address, waittime string) (err error) {
	defer func() {
		// This should never happen, but we don't a failed connect to kill us
		if r := recover(); r != nil {
			err = fmt.Errorf("panic in connection attempt:\n%v", r)
		}
	}()

	i, err := strconv.ParseInt(waittime, 10, 64)
	if err != nil {
		return err
	}
	time.Sleep(time.Duration(i) * time.Second)
	return c.Connect(address)
}

// Authorize against stratum pool
func (c *Client) Authorize(username, password, invitecode, payoutaddress string) error {
	req := AuthorizeRequest(username, password, invitecode, payoutaddress)
	c.Lock()
	c.requestsMade[req.ID] = func(resp Response) {
		var result bool
		if err := resp.FitResult(&result); err == nil {
			if result == false {
				log.Errorf("AuthorizeResponse is false. Rather than contributing uncredited mining, shutting down client.")
				c.Close()
			} else {
				log.Infof("AuthorizeResponse result: %t\n", result)
			}
		}
	}
	c.Unlock()
	err := c.Encode(req)
	if err != nil {
		return err
	}

	return nil
}

// Request current OPR hash from server
func (c *Client) GetOPRHash(jobID string) error {
	req := GetOPRHashRequest(jobID)
	c.Lock()
	c.requestsMade[req.ID] = func(resp Response) {
		var result string
		if err := resp.FitResult(&result); err == nil {
			log.Infof("OPRHash result: %s\n", result)
			if jobID == c.currentJobID {
				newOPRHash, err := hex.DecodeString(result)
				if err != nil {
					log.Error(err)
					return
				}
				command := mining.BuildCommand().
					ResetRecords().         // Reset nonce to keep it small
					NewOPRHash(newOPRHash). // New OPR hash to mine
					ResumeMining().         // Start mining
					Build()
				c.SendCommand(command)

			}
		}
	}
	c.Unlock()
	err := c.Encode(req)
	if err != nil {
		return err
	}
	return nil
}

// Submit completed work to server
func (c *Client) Submit(username, jobID, nonce, oprHash, target string) error {
	req := SubmitRequest(username, jobID, nonce, oprHash, target)
	c.Lock()
	c.requestsMade[req.ID] = func(resp Response) {
		var result bool
		if err := resp.FitResult(&result); err == nil {
			log.WithFields(log.Fields{
				"nonce":   nonce,
				"oprhash": oprHash,
				"target":  target,
			}).Tracef("Submission result: %t\n", result)
		}
	}
	c.Unlock()
	err := c.Encode(req)
	if err != nil {
		return err
	}
	return nil
}

// Subscribe to stratum pool
func (c *Client) Subscribe() error {
	req := SubscribeRequest(c.version)
	c.Lock()
	c.requestsMade[req.ID] = func(resp Response) {
		var subscriptions []Subscription
		if err := resp.FitResult(&subscriptions); err == nil {
			log.Println("Subscriptions Results:")
			for _, subscription := range subscriptions {
				log.Println("...", subscription)

				// Set nonce if provided here
				if subscription.Type == "mining.set_nonce" {
					nonce, err := strconv.ParseUint(subscription.Id, 10, 32)
					if err != nil {
						log.Errorln("Nonce unable to be converted to integer: ", err)
						continue
					}

					c.SetNewNonce(uint32(nonce))
				}
			}
		} else {
			log.Error(err)
		}
	}
	c.Unlock()
	err := c.Encode(req)
	if err != nil {
		return err
	}
	return nil
}

// Suggest preferred mining target to server
func (c *Client) SuggestTarget(preferredTarget string) error {
	err := c.Encode(SuggestTargetRequest(preferredTarget))
	if err != nil {
		return err
	}
	return nil
}

func (c *Client) Close() error {
	c.autoreconnect = false
	if !reflect.ValueOf(c.conn).IsNil() {
		log.Infof("shutting down stratum client")
		return c.conn.Close()
	}
	return nil
}

func (c *Client) Listen(ctx context.Context) {
	// Capture a cancel and close the client
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
			log.Infof("Graceful close of the miner connection")
			return
		}
	}()

	log.Printf("Stratum client listening to server at %s\n", c.conn.RemoteAddr().String())
	originalServerAddress := c.conn.RemoteAddr().String()

	r := bufio.NewReader(c.conn)

	for {
		readBytes, _, err := r.ReadLine()
		if err != nil {
			if !c.autoreconnect {
				return // Stop trying to reconnect
			}
			_ = c.conn.Close()
			log.WithError(err).Errorf("client lost connection to the server, reconnect attempt in 5s")
			err := c.BlockTillConnected(originalServerAddress, "5")
			if err != nil {
				log.WithError(err).Error("miner reconnect failed")
				return
			}

			_ = c.Handshake()
			r = bufio.NewReader(c.conn)
		} else {
			c.HandleMessage(readBytes)
		}
	}
}

func (c *Client) HandleMessage(data []byte) {
	var u UnknownRPC
	err := json.Unmarshal(data, &u)
	if err != nil {
		log.WithError(err).Warnf("client read failed")
	}

	if u.IsRequest() {
		req := u.GetRequest()
		c.HandleRequest(req)
	} else {
		resp := u.GetResponse()
		c.HandleResponse(resp)
	}

	// TODO: Don't just print everything
	//log.Infof(string(data))
}

func (c *Client) HandleRequest(req Request) {
	var params RPCParams
	if err := req.FitParams(&params); err != nil {
		log.WithField("method", req.Method).Warnf("bad params %s", req.Method)
		return
	}

	switch req.Method {
	case "client.get_version":
		if err := c.Encode(GetVersionResponse(req.ID, c.version)); err != nil {
			log.WithField("method", req.Method).WithError(err).Error("failed to respond to get_version")
		}
	case "client.reconnect":
		if len(params) < 2 {
			log.Errorf("Not enough parameters to reconnect with: %s\n", params)
			return
		}

		waittime := "0"
		if len(params) > 2 {
			_, err := strconv.ParseInt(params[2], 10, 64)
			if err == nil {
				waittime = params[2]
			}
		}

		if err := c.WaitThenConnect(params[0]+":"+params[1], waittime); err != nil {
			log.WithField("method", req.Method).WithError(err).Error("failed to reconnect")
		}
	case "client.show_message":
		if len(params) < 1 {
			log.Errorln("No message to show")
			return
		}
		// Print & log message in human-readable way
		fmt.Printf("\n\nMessage from server: %s\n\n\n", params[0])
		//log.Printf("Message from server: %s\n", params[0])
	case "mining.notify":
		if len(params) < 2 {
			log.Errorf("Not enough parameters from notify: %s\n", params)
			return
		}

		jobID := params[0]
		oprHash := params[1]

		newJobID, err := strconv.ParseInt(jobID, 10, 64)
		if err != nil {
			log.Error("Not a valid new JobID")
			return
		}
		existingJobID, _ := strconv.ParseInt(c.currentJobID, 10, 64)
		if newJobID >= existingJobID {
			myHexBytes, err := hex.DecodeString(oprHash)
			if err != nil {
				log.Error(err)
				return
			}
			if newJobID > existingJobID {
				c.currentJobID = jobID
			}
			c.currentOPRHash = oprHash
			stats := make(chan *mining.SingleMinerStats, len(c.miners))
			command := mining.BuildCommand().
				SubmitStats(stats).
				ResetRecords().
				NewOPRHash(myHexBytes).
				MinimumDifficulty(c.currentTarget).
				ResumeMining().
				Build()
			c.SendCommand(command)

			go c.AggregateStats(int32(existingJobID), stats, len(c.miners))

			log.Printf("JobID: %s ... OPR Hash: %s\n", jobID, oprHash)
		} else {
			log.WithError(fmt.Errorf("old job")).Warnf("Rejected JobID: %s ... OPR Hash: %s\n", jobID, oprHash)
		}
	case "mining.set_target":
		if len(params) < 1 {
			log.Errorf("Not enough parameters from set_target: %s\n", params)
			return
		}

		result, err := strconv.ParseUint(strings.Replace(params[0], "0x", "", -1), 16, 64)
		if err != nil {
			log.Errorln("Target unable to be converted to uint: ", err)
			return
		}
		c.currentTarget = uint64(result)

		log.Printf("New Target: %x\n", c.currentTarget)

		command := mining.BuildCommand().
			MinimumDifficulty(c.currentTarget).
			Build()
		c.SendCommand(command)
	case "mining.set_nonce":
		if len(params) < 1 {
			log.Errorf("Not enough parameters from set_nonce: %s\n", params)
			return
		}

		nonceString := params[0]
		nonce, err := strconv.ParseUint(nonceString, 10, 32)
		if err != nil {
			log.Errorln("Nonce unable to be converted to integer: ", err)
			return
		}

		c.SetNewNonce(uint32(nonce))
	case "mining.stop_mining":
		log.Println("Request to stop mining received")
		command := mining.BuildCommand().
			PauseMining().
			Build()
		c.SendCommand(command)
	default:
		log.Warnf("unknown method %s", req.Method)
	}
}

func (c *Client) SetNewNonce(nonce uint32) {
	log.Printf("New Nonce: %d\n", nonce)
	command := mining.BuildCommand().
		NewNoncePrefix(uint32(nonce)).
		Build()
	c.SendCommand(command)
}

func (c *Client) AggregateStats(job int32, stats chan *mining.SingleMinerStats, l int) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel() // Must clean up context to avoid a memory leak
	groupStats := mining.NewGroupMinerStats(job)

	for i := 0; i < l; i++ {
		select {
		case stat := <-stats:
			groupStats.Miners[stat.ID] = stat
		case <-ctx.Done():
		}
	}

	log.WithFields(groupStats.LogFields()).Info("job miner stats")
}

func (c *Client) HandleResponse(resp Response) {
	c.Lock()
	if funcToPerform, ok := c.requestsMade[resp.ID]; ok {
		funcToPerform(resp)
		delete(c.requestsMade, resp.ID)
	} else {
		log.Errorf("Response received for unrecognized request ID: %d (ignoring)\n", resp.ID)
	}
	c.Unlock()
}

func (c *Client) ListenForSuccess() {
	for {
		select {
		case winner := <-c.successes:
			if float64(len(c.successes))/float64(cap(c.successes)) > 0.9 {
				log.Warnf("success channel is at %d/%d", len(c.successes), cap(c.successes))
			}
			err := c.Submit(c.username, c.currentJobID, winner.Nonce, winner.OPRHash, winner.Target)
			if err != nil {
				log.WithError(err).Error("failed to submit to server")
			} else {
				c.totalSuccesses++
			}
		}
	}
}

func (c *Client) TotalSuccesses() uint64 {
	return c.totalSuccesses
}
