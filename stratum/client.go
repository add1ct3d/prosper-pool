package stratum

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
)

var _ = log.Println

// Clients talk to stratum servers. They are on the miner side of things, so their config's
// should be extremely light, if any.
type Client struct {
	enc  *json.Encoder
	dec  *bufio.Reader
	conn net.Conn

	version string

	subscriptions []Subscription
	verbose       bool
}

func NewClient(verbose bool) (*Client, error) {
	c := new(Client)
	c.verbose = verbose
	c.version = "0.0.1"
	return c, nil
}

func (c *Client) Connect(address string) error {
	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return err
	}

	conn, err := net.DialTCP("tcp", nil, addr)
	if err != nil {
		return err
	}

	return c.Handshake(conn)
}

func (c *Client) Handshake(conn net.Conn) error {
	c.InitConn(conn)
	err := c.Subscribe()
	if err != nil {
		return err
	}

	// Receive subscribe response
	data, _, err := c.dec.ReadLine()
	var resp Response
	err = json.Unmarshal(data, &resp)

	if c.verbose {
		log.Printf("CLIENT READ: %s\n", string(data))
	}

	err = c.Authorize("user", "password")
	if err != nil {
		return err
	}

	data, _, err = c.dec.ReadLine()
	err = json.Unmarshal(data, &resp)
	if c.verbose {
		log.Printf("CLIENT READ: %s\n", string(data))
	}
	return nil
}

// JustConnect will not start the handshake process. Good for unit tests
func (c *Client) InitConn(conn net.Conn) {
	c.conn = conn
	c.enc = json.NewEncoder(conn)
	c.dec = bufio.NewReader(conn)
}

// Authorize against stratum pool
func (c Client) Authorize(username, password string) error {
	err := c.enc.Encode(AuthorizeRequest(username, password))
	if err != nil {
		return err
	}
	return nil
}

// Request current OPR hash from server
func (c Client) GetOPRHash(jobID string) error {
	err := c.enc.Encode(GetOPRHashRequest(jobID))
	if err != nil {
		return err
	}
	return nil
}

// Submit completed work to server
func (c Client) Submit(username, jobID, nonce, oprHash string) error {
	err := c.enc.Encode(SubmitRequest(username, jobID, nonce, oprHash))
	if err != nil {
		return err
	}
	return nil
}

// Subscribe to stratum pool
func (c Client) Subscribe() error {
	err := c.enc.Encode(SubscribeRequest())
	if err != nil {
		return err
	}
	return nil
}

// Suggest preferred mining difficulty to server
func (c Client) SuggestDifficulty(preferredDifficulty string) error {
	err := c.enc.Encode(SuggestDifficultyRequest(preferredDifficulty))
	if err != nil {
		return err
	}
	return nil
}

func (c Client) Listen(ctx context.Context) {
	defer c.conn.Close()
	// Capture a cancel and close the server
	go func() {
		select {
		case <-ctx.Done():
			log.Infof("shutting down stratum client")
			c.conn.Close()
			return
		}
	}()

	log.Printf("Stratum client listening to server at %s\n", c.conn.RemoteAddr().String())

	r := bufio.NewReader(c.conn)

	for {
		readBytes, _, err := r.ReadLine()
		if err != nil {
			return
		} else {
			c.HandleMessage(readBytes)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (c Client) HandleMessage(data []byte) {
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
		// TODO: Handle resp
		var _ = resp
	}

	// TODO: Don't just print everything
	log.Infof(string(data))
}

func (c Client) HandleRequest(req Request) {
	var params RPCParams
	switch req.Method {
	case "client.get_version":
		if err := req.FitParams(&params); err != nil {
			log.WithField("method", req.Method).Warnf("bad params %s", req.Method)
			return
		}

		if err := c.enc.Encode(GetVersionResponse(req.ID, c.version)); err != nil {
			log.WithField("method", req.Method).WithError(err).Error("failed to send message")
		}
	default:
		log.Warnf("unknown method %s", req.Method)
	}
}
