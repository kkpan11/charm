package charm

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"regexp"

	"github.com/meowgorithm/babyenv"
	"github.com/mitchellh/go-homedir"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var nameValidator = regexp.MustCompile("^[a-zA-Z0-9]{1,50}$")

var ErrMissingSSHAuth = errors.New("missing ssh auth")

var ErrNameTaken = errors.New("name already taken")

var ErrNameInvalid = errors.New("invalid name")

type Config struct {
	IDHost      string `env:"CHARM_ID_HOST" default:"id.dev.charm.sh"`
	IDPort      int    `env:"CHARM_ID_PORT" default:"5555"`
	BioHost     string `env:"CHARM_BIO_HOST" default:"http://bio.dev.charm.sh"`
	BioPort     int    `env:"CHARM_BIO_PORT" default:"80"`
	UseSSHAgent bool   `env:"CHARM_USE_SSH_AGENT" default:"true"`
	SSHKeyPath  string `env:"CHARM_SSH_KEY_PATH" default:"~/.ssh/id_dsa"`
	ForceKey    bool
}

type Client struct {
	config    *Config
	sshConfig *ssh.ClientConfig
	session   *ssh.Session
	User      *User
}

type User struct {
	CharmID string `json:"charm_id"`
	Name    string `json:"name"`
}

type sshSession struct {
	session *ssh.Session
}

func ConfigFromEnv() (*Config, error) {
	var cfg Config
	if err := babyenv.Parse(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func NewClient(cfg *Config) (*Client, error) {
	cc := &Client{config: cfg}
	if !cfg.ForceKey {
		am, err := agentAuthMethod()
		if err == nil {
			cc.sshConfig = &ssh.ClientConfig{
				User:            "charm",
				Auth:            []ssh.AuthMethod{am},
				HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			}
			cc.session, err = cc.sshSession()
			if err == nil {
				return cc, nil
			}
		}
	}

	var pkam ssh.AuthMethod
	// fmt.Printf("Using SSH key %s\n", cfg.SSHKeyPath)
	pkam, err := publicKeyAuthMethod(cfg.SSHKeyPath)
	if err != nil {
		// fmt.Printf("Couldn't find SSH key %s, trying ~/.ssh/id_rsa\n", cfg.SSHKeyPath)
		pkam, err = publicKeyAuthMethod("~/.ssh/id_rsa")
		if err != nil {
			return nil, ErrMissingSSHAuth
		}
	}
	cc.sshConfig = &ssh.ClientConfig{
		User:            "charm",
		Auth:            []ssh.AuthMethod{pkam},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}
	cc.session, err = cc.sshSession()
	if err != nil {
		return nil, err
	}
	return cc, nil
}

func (cc *Client) JWT() (string, error) {
	defer cc.session.Close()
	jwt, err := cc.session.Output("jwt")
	if err != nil {
		return "", err
	}
	return string(jwt), nil
}

func (cc *Client) ID() (string, error) {
	defer cc.session.Close()
	id, err := cc.session.Output("id")
	if err != nil {
		return "", err
	}
	return string(id), nil
}

func (cc *Client) AuthorizedKeys() (string, error) {
	defer cc.session.Close()
	jwt, err := cc.session.Output("keys")
	if err != nil {
		return "", err
	}
	return string(jwt), nil
}

func (cc *Client) Link(lh LinkHandler, code string) error {
	defer cc.session.Close()
	out, err := cc.session.StdoutPipe()
	if err != nil {
		return err
	}

	err = cc.session.Start(fmt.Sprintf("api-link %s", code))
	if err != nil {
		return err
	}
	var lr Link
	dec := json.NewDecoder(out)
	err = dec.Decode(&lr)
	if err != nil {
		return err
	}
	if !checkLinkStatus(lh, &lr) {
		return nil
	}

	var lr2 Link
	err = dec.Decode(&lr2)
	if err != nil {
		return err
	}
	if !checkLinkStatus(lh, &lr2) {
		return nil
	}

	var lr3 Link
	err = dec.Decode(&lr3)
	if err != nil {
		return err
	}
	if !checkLinkStatus(lh, &lr3) {
		return nil
	}
	return nil
}

func (cc *Client) LinkGen(lh LinkHandler) error {
	defer cc.session.Close()
	out, err := cc.session.StdoutPipe()
	if err != nil {
		return err
	}
	in, err := cc.session.StdinPipe()
	if err != nil {
		return err
	}

	err = cc.session.Start("api-link")
	if err != nil {
		return err
	}

	// initialize link request on server
	var lr Link
	dec := json.NewDecoder(out)
	err = dec.Decode(&lr)
	if err != nil {
		return err
	}
	if !checkLinkStatus(lh, &lr) {
		return nil
	}

	// waiting for link request, do we want to approve it?
	var lr2 Link
	err = dec.Decode(&lr2)
	if err != nil {
		return err
	}
	if !checkLinkStatus(lh, &lr2) {
		return nil
	}

	// send approval response
	var lm LinkerMessage
	enc := json.NewEncoder(in)
	if lh.Request(&lr2) {
		lm = LinkerMessage{"yes"}
	} else {
		lm = LinkerMessage{"no"}
	}
	err = enc.Encode(lm)
	if err != nil {
		return err
	}
	if lm.Message == "no" {
		return nil
	}

	// get server response
	var lr3 Link
	err = dec.Decode(&lr3)
	if err != nil {
		return err
	}
	checkLinkStatus(lh, &lr3)
	return nil
}

func (cc *Client) SetName(name string) (*User, error) {
	if !ValidateName(name) {
		return nil, ErrNameInvalid
	}
	u := &User{}
	u.Name = name
	client := &http.Client{}
	buf := &bytes.Buffer{}
	err := json.NewEncoder(buf).Encode(u)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", fmt.Sprintf("%s:%d/bio", cc.config.BioHost, cc.config.BioPort), buf)
	if err != nil {
		return nil, err
	}
	jwt, err := cc.JWT()
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", fmt.Sprintf("bearer %s", jwt))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 409 {
		return nil, ErrNameTaken
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("server error")
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&u)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (cc *Client) Bio() (*User, error) {
	u := &User{}
	client := &http.Client{}
	buf := &bytes.Buffer{}
	err := json.NewEncoder(buf).Encode(u)
	if err != nil {
		return nil, err
	}
	id, err := cc.ID()
	if err != nil {
		return nil, err
	}
	err = cc.RenewSession()
	if err != nil {
		return nil, err
	}
	jwt, err := cc.JWT()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s:%d/id/%s", cc.config.BioHost, cc.config.BioPort, id), buf)
	if err != nil {
		return nil, err
	}
	req.Header.Add("Authorization", fmt.Sprintf("bearer %s", jwt))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 409 {
		return nil, ErrNameTaken
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("server error")
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	err = dec.Decode(&u)
	if err != nil {
		return nil, err
	}
	return u, nil
}

// RenewSession resets the session so we can perform another SSH-backed command
func (cc *Client) RenewSession() error {
	var err error
	cc.session, err = cc.sshSession()
	return err
}

// CloseSession closes the client's SSH session
func (cc *Client) CloseSession() error {
	return cc.session.Close()
}

// ValidateName validates a given name
func ValidateName(name string) bool {
	return nameValidator.MatchString(name)
}

func (cc *Client) sshSession() (*ssh.Session, error) {
	c, err := ssh.Dial("tcp", fmt.Sprintf("%s:%d", cc.config.IDHost, cc.config.IDPort), cc.sshConfig)
	if err != nil {
		return nil, err
	}
	s, err := c.NewSession()
	if err != nil {
		return nil, err
	}
	return s, nil
}

func fileExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return true, err
}

func publicKeyAuthMethod(kp string) (ssh.AuthMethod, error) {
	keyPath, err := homedir.Expand(kp)
	if err != nil {
		return nil, err
	}
	key, err := ioutil.ReadFile(keyPath)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, err
	}
	return ssh.PublicKeys(signer), nil
}

func agentAuthMethod() (ssh.AuthMethod, error) {
	socket := os.Getenv("SSH_AUTH_SOCK")
	if socket == "" {
		// fmt.Println("No SSH_AUTH_SOCK set, not using ssh-agent")
		return nil, fmt.Errorf("Missing socket env var")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		// fmt.Printf("SSH agent dial error: %s\n", err)
		return nil, err
	}
	agentClient := agent.NewClient(conn)
	return ssh.PublicKeysCallback(agentClient.Signers), nil
}
