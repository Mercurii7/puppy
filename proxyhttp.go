package main

/*
Wrappers around http.Request and http.Response to add helper functions needed by the proxy
*/

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/deckarep/golang-set"
	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

const (
	ToServer = iota
	ToClient
)

type NetDialer func(network, addr string) (net.Conn, error)

type ProxyResponse struct {
	http.Response
	bodyBytes []byte
	DbId      string // ID used by storage implementation. Blank string = unsaved
	Unmangled *ProxyResponse
}

type ProxyRequest struct {
	http.Request

	// Destination connection info
	DestHost   string
	DestPort   int
	DestUseTLS bool

	// Associated messages
	ServerResponse *ProxyResponse
	WSMessages     []*ProxyWSMessage
	Unmangled      *ProxyRequest

	// Additional data
	bodyBytes     []byte
	DbId          string // ID used by storage implementation. Blank string = unsaved
	StartDatetime time.Time
	EndDatetime   time.Time

	tags mapset.Set

	NetDial NetDialer
}

type WSSession struct {
	websocket.Conn

	Request *ProxyRequest // Request used for handshake
}

type ProxyWSMessage struct {
	Type      int
	Message   []byte
	Direction int
	Unmangled *ProxyWSMessage
	Timestamp time.Time
	Request   *ProxyRequest

	DbId string // ID used by storage implementation. Blank string = unsaved
}

func PerformConnect(conn net.Conn, destHost string, destPort int) error {
	connStr := []byte(fmt.Sprintf("CONNECT %s:%d HTTP/1.1\r\nHost: %s\r\nProxy-Connection: Keep-Alive\r\n\r\n", destHost, destPort, destHost))
	conn.Write(connStr)
	rsp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return fmt.Errorf("error performing CONNECT handshake: %s", err.Error())
	}
	if rsp.StatusCode != 200 {
		return fmt.Errorf("error performing CONNECT handshake")
	}
	return nil
}

func NewProxyRequest(r *http.Request, destHost string, destPort int, destUseTLS bool) *ProxyRequest {
	var retReq *ProxyRequest
	if r != nil {
		// Write/reread the request to make sure we get all the extra headers Go adds into req.Header
		buf := bytes.NewBuffer(make([]byte, 0))
		r.Write(buf)
		httpReq2, err := http.ReadRequest(bufio.NewReader(buf))
		if err != nil {
			panic(err)
		}

		retReq = &ProxyRequest{
			*httpReq2,
			destHost,
			destPort,
			destUseTLS,
			nil,
			make([]*ProxyWSMessage, 0),
			nil,
			make([]byte, 0),
			"",
			time.Unix(0, 0),
			time.Unix(0, 0),
			mapset.NewSet(),
			nil,
		}
	} else {
		newReq, _ := http.NewRequest("GET", "/", nil) // Ignore error since this should be run the same every time and shouldn't error
		newReq.Header.Set("User-Agent", "Puppy-Proxy/1.0")
		newReq.Host = destHost
		retReq = &ProxyRequest{
			*newReq,
			destHost,
			destPort,
			destUseTLS,
			nil,
			make([]*ProxyWSMessage, 0),
			nil,
			make([]byte, 0),
			"",
			time.Unix(0, 0),
			time.Unix(0, 0),
			mapset.NewSet(),
			nil,
		}
	}

	// Load the body
	bodyBuf, _ := ioutil.ReadAll(retReq.Body)
	retReq.SetBodyBytes(bodyBuf)
	return retReq
}

func ProxyRequestFromBytes(b []byte, destHost string, destPort int, destUseTLS bool) (*ProxyRequest, error) {
	buf := bytes.NewBuffer(b)
	httpReq, err := http.ReadRequest(bufio.NewReader(buf))
	if err != nil {
		return nil, err
	}

	return NewProxyRequest(httpReq, destHost, destPort, destUseTLS), nil
}

func NewProxyResponse(r *http.Response) *ProxyResponse {
	// Write/reread the request to make sure we get all the extra headers Go adds into req.Header
	oldClose := r.Close
	r.Close = false
	buf := bytes.NewBuffer(make([]byte, 0))
	r.Write(buf)
	r.Close = oldClose
	httpRsp2, err := http.ReadResponse(bufio.NewReader(buf), nil)
	if err != nil {
		panic(err)
	}
	httpRsp2.Close = false
	retRsp := &ProxyResponse{
		*httpRsp2,
		make([]byte, 0),
		"",
		nil,
	}

	bodyBuf, _ := ioutil.ReadAll(retRsp.Body)
	retRsp.SetBodyBytes(bodyBuf)
	return retRsp
}

func ProxyResponseFromBytes(b []byte) (*ProxyResponse, error) {
	buf := bytes.NewBuffer(b)
	httpRsp, err := http.ReadResponse(bufio.NewReader(buf), nil)
	if err != nil {
		return nil, err
	}
	return NewProxyResponse(httpRsp), nil
}

func NewProxyWSMessage(mtype int, message []byte, direction int) (*ProxyWSMessage, error) {
	return &ProxyWSMessage{
		Type:      mtype,
		Message:   message,
		Direction: direction,
		Unmangled: nil,
		Timestamp: time.Unix(0, 0),
		DbId:      "",
	}, nil
}

func (req *ProxyRequest) DestScheme() string {
	if req.IsWSUpgrade() {
		if req.DestUseTLS {
			return "wss"
		} else {
			return "ws"
		}
	} else {
		if req.DestUseTLS {
			return "https"
		} else {
			return "http"
		}
	}
}

func (req *ProxyRequest) FullURL() *url.URL {
	// Same as req.URL but guarantees it will include the scheme, host, and port if necessary

	var u url.URL
	u = *(req.URL) // Copy the original req.URL
	u.Host = req.Host
	u.Scheme = req.DestScheme()
	return &u
}

func (req *ProxyRequest) DestURL() *url.URL {
	// Same as req.FullURL() but uses DestHost and DestPort for the host and port

	var u url.URL
	u = *(req.URL) // Copy the original req.URL
	u.Scheme = req.DestScheme()

	if req.DestUseTLS && req.DestPort == 443 ||
		!req.DestUseTLS && req.DestPort == 80 {
		u.Host = req.DestHost
	} else {
		u.Host = fmt.Sprintf("%s:%d", req.DestHost, req.DestPort)
	}
	return &u
}

func (req *ProxyRequest) Submit(conn net.Conn) error {
	return req.submit(conn, false, nil)
}

func (req *ProxyRequest) SubmitProxy(conn net.Conn, creds *ProxyCredentials) error {
	return req.submit(conn, true, creds)
}

func (req *ProxyRequest) submit(conn net.Conn, forProxy bool, proxyCreds *ProxyCredentials) error {
	// Write the request to the connection
	req.StartDatetime = time.Now()
	if forProxy {
		if req.DestUseTLS {
			req.URL.Scheme = "https"
		} else {
			req.URL.Scheme = "http"
		}
		req.URL.Opaque = ""

		if err := req.RepeatableProxyWrite(conn, proxyCreds); err != nil {
			return err
		}
	} else {
		if err := req.RepeatableWrite(conn); err != nil {
			return err
		}
	}

	// Read a response from the server
	httpRsp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return fmt.Errorf("error reading response: %s", err.Error())
	}
	req.EndDatetime = time.Now()

	prsp := NewProxyResponse(httpRsp)
	req.ServerResponse = prsp
	return nil
}

func (req *ProxyRequest) WSDial(conn net.Conn) (*WSSession, error) {
	if !req.IsWSUpgrade() {
		return nil, fmt.Errorf("could not start websocket session: request is not a websocket handshake request")
	}

	upgradeHeaders := make(http.Header)
	for k, v := range req.Header {
		for _, vv := range v {
			if !(k == "Upgrade" ||
				k == "Connection" ||
				k == "Sec-Websocket-Key" ||
				k == "Sec-Websocket-Version" ||
				k == "Sec-Websocket-Extensions" ||
				k == "Sec-Websocket-Protocol") {
				upgradeHeaders.Add(k, vv)
			}
		}
	}

	dialer := &websocket.Dialer{}
	dialer.NetDial = func(network, address string) (net.Conn, error) {
		return conn, nil
	}

	wsconn, rsp, err := dialer.Dial(req.DestURL().String(), upgradeHeaders)
	if err != nil {
		return nil, fmt.Errorf("could not dial WebSocket server: %s", err)
	}
	req.ServerResponse = NewProxyResponse(rsp)
	wsession := &WSSession{
		*wsconn,
		req,
	}
	return wsession, nil
}

func WSDial(req *ProxyRequest) (*WSSession, error) {
	return wsDial(req, false, "", 0, nil, false)
}

func WSDialProxy(req *ProxyRequest, proxyHost string, proxyPort int, creds *ProxyCredentials) (*WSSession, error) {
	return wsDial(req, true, proxyHost, proxyPort, creds, false)
}

func WSDialSOCKSProxy(req *ProxyRequest, proxyHost string, proxyPort int, creds *ProxyCredentials) (*WSSession, error) {
	return wsDial(req, true, proxyHost, proxyPort, creds, true)
}

func wsDial(req *ProxyRequest, useProxy bool, proxyHost string, proxyPort int, proxyCreds *ProxyCredentials, proxyIsSOCKS bool) (*WSSession, error) {
	var conn net.Conn
	var dialer NetDialer
	var err error

	if req.NetDial != nil {
		dialer = req.NetDial
	} else {
		dialer = net.Dial
	}

	if useProxy {
		if proxyIsSOCKS {
			var socksCreds *proxy.Auth
			if proxyCreds != nil {
				socksCreds = &proxy.Auth{
					User:     proxyCreds.Username,
					Password: proxyCreds.Password,
				}
			}
			socksDialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("%s:%d", proxyHost, proxyPort), socksCreds, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("error creating SOCKS dialer: %s", err.Error())
			}
			conn, err = socksDialer.Dial("tcp", fmt.Sprintf("%s:%d", req.DestHost, req.DestPort))
			if err != nil {
				return nil, fmt.Errorf("error dialing host: %s", err.Error())
			}
			defer conn.Close()
		} else {
			conn, err = dialer("tcp", fmt.Sprintf("%s:%d", proxyHost, proxyPort))
			if err != nil {
				return nil, fmt.Errorf("error dialing proxy: %s", err.Error())
			}

			// always perform a CONNECT for websocket regardless of SSL
			if err := PerformConnect(conn, req.DestHost, req.DestPort); err != nil {
				return nil, err
			}
		}
	} else {
		conn, err = dialer("tcp", fmt.Sprintf("%s:%d", req.DestHost, req.DestPort))
		if err != nil {
			return nil, fmt.Errorf("error dialing host: %s", err.Error())
		}
	}

	if req.DestUseTLS {
		tls_conn := tls.Client(conn, &tls.Config{
			InsecureSkipVerify: true,
		})
		conn = tls_conn
	}

	return req.WSDial(conn)
}

func (req *ProxyRequest) IsWSUpgrade() bool {
	for k, v := range req.Header {
		for _, vv := range v {
			if strings.ToLower(k) == "upgrade" && strings.Contains(vv, "websocket") {
				return true
			}
		}
	}
	return false
}

func (req *ProxyRequest) StripProxyHeaders() {
	if !req.IsWSUpgrade() {
		req.Header.Del("Connection")
	}
	req.Header.Del("Accept-Encoding")
	req.Header.Del("Proxy-Connection")
	req.Header.Del("Proxy-Authenticate")
	req.Header.Del("Proxy-Authorization")
}

func (req *ProxyRequest) Eq(other *ProxyRequest) bool {
	if req.StatusLine() != other.StatusLine() ||
		!reflect.DeepEqual(req.Header, other.Header) ||
		bytes.Compare(req.BodyBytes(), other.BodyBytes()) != 0 ||
		req.DestHost != other.DestHost ||
		req.DestPort != other.DestPort ||
		req.DestUseTLS != other.DestUseTLS {
		return false
	}

	return true
}

func (req *ProxyRequest) Clone() *ProxyRequest {
	buf := bytes.NewBuffer(make([]byte, 0))
	req.RepeatableWrite(buf)
	newReq, err := ProxyRequestFromBytes(buf.Bytes(), req.DestHost, req.DestPort, req.DestUseTLS)
	if err != nil {
		panic(err)
	}
	newReq.DestHost = req.DestHost
	newReq.DestPort = req.DestPort
	newReq.DestUseTLS = req.DestUseTLS
	newReq.Header = CopyHeader(req.Header)
	return newReq
}

func (req *ProxyRequest) DeepClone() *ProxyRequest {
	// Returns a request with the same request, response, and associated websocket messages
	newReq := req.Clone()
	newReq.DbId = req.DbId

	if req.Unmangled != nil {
		newReq.Unmangled = req.Unmangled.DeepClone()
	}

	if req.ServerResponse != nil {
		newReq.ServerResponse = req.ServerResponse.DeepClone()
	}

	for _, wsm := range req.WSMessages {
		newReq.WSMessages = append(newReq.WSMessages, wsm.DeepClone())
	}

	return newReq
}

func (req *ProxyRequest) resetBodyReader() {
	// yes I know this method isn't the most efficient, I'll fix it if it causes problems later
	req.Body = ioutil.NopCloser(bytes.NewBuffer(req.BodyBytes()))
}

func (req *ProxyRequest) RepeatableWrite(w io.Writer) error {
	defer req.resetBodyReader()
	return req.Write(w)
}

func (req *ProxyRequest) RepeatableProxyWrite(w io.Writer, proxyCreds *ProxyCredentials) error {
	defer req.resetBodyReader()
	if proxyCreds != nil {
		authHeader := proxyCreds.SerializeHeader()
		req.Header.Set("Proxy-Authorization", authHeader)
		defer func() { req.Header.Del("Proxy-Authorization") }()
	}
	return req.WriteProxy(w)
}

func (req *ProxyRequest) BodyBytes() []byte {
	return DuplicateBytes(req.bodyBytes)

}

func (req *ProxyRequest) SetBodyBytes(bs []byte) {
	req.bodyBytes = bs
	req.resetBodyReader()

	// Parse the form if we can, ignore errors
	req.ParseMultipartForm(1024 * 1024 * 1024) // 1GB for no good reason
	req.ParseForm()
	req.resetBodyReader()
	req.Header.Set("Content-Length", strconv.Itoa(len(bs)))
}

func (req *ProxyRequest) FullMessage() []byte {
	buf := bytes.NewBuffer(make([]byte, 0))
	req.RepeatableWrite(buf)
	return buf.Bytes()
}

func (req *ProxyRequest) PostParameters() (url.Values, error) {
	vals, err := url.ParseQuery(string(req.BodyBytes()))
	if err != nil {
		return nil, err
	}
	return vals, nil
}

func (req *ProxyRequest) SetPostParameter(key string, value string) {
	req.PostForm.Set(key, value)
	req.SetBodyBytes([]byte(req.PostForm.Encode()))
}

func (req *ProxyRequest) AddPostParameter(key string, value string) {
	req.PostForm.Add(key, value)
	req.SetBodyBytes([]byte(req.PostForm.Encode()))
}

func (req *ProxyRequest) DeletePostParameter(key string, value string) {
	req.PostForm.Del(key)
	req.SetBodyBytes([]byte(req.PostForm.Encode()))
}

func (req *ProxyRequest) SetURLParameter(key string, value string) {
	q := req.URL.Query()
	q.Set(key, value)
	req.URL.RawQuery = q.Encode()
	req.ParseForm()
}

func (req *ProxyRequest) URLParameters() url.Values {
	vals := req.URL.Query()
	return vals
}

func (req *ProxyRequest) AddURLParameter(key string, value string) {
	q := req.URL.Query()
	q.Add(key, value)
	req.URL.RawQuery = q.Encode()
	req.ParseForm()
}

func (req *ProxyRequest) DeleteURLParameter(key string, value string) {
	q := req.URL.Query()
	q.Del(key)
	req.URL.RawQuery = q.Encode()
	req.ParseForm()
}

func (req *ProxyRequest) AddTag(tag string) {
	req.tags.Add(tag)
}

func (req *ProxyRequest) CheckTag(tag string) bool {
	return req.tags.Contains(tag)
}

func (req *ProxyRequest) RemoveTag(tag string) {
	req.tags.Remove(tag)
}

func (req *ProxyRequest) ClearTags() {
	req.tags.Clear()
}

func (req *ProxyRequest) Tags() []string {
	items := req.tags.ToSlice()
	retslice := make([]string, 0)
	for _, item := range items {
		str, ok := item.(string)
		if ok {
			retslice = append(retslice, str)
		}
	}
	return retslice
}

func (req *ProxyRequest) HTTPPath() string {
	// The path used in the http request
	u := *req.URL
	u.Scheme = ""
	u.Host = ""
	u.Opaque = ""
	u.User = nil
	return u.String()
}

func (req *ProxyRequest) StatusLine() string {
	return fmt.Sprintf("%s %s %s", req.Method, req.HTTPPath(), req.Proto)
}

func (req *ProxyRequest) HeaderSection() string {
	retStr := req.StatusLine()
	retStr += "\r\n"
	for k, vs := range req.Header {
		for _, v := range vs {
			retStr += fmt.Sprintf("%s: %s\r\n", k, v)
		}
	}
	return retStr
}

func (rsp *ProxyResponse) resetBodyReader() {
	// yes I know this method isn't the most efficient, I'll fix it if it causes problems later
	rsp.Body = ioutil.NopCloser(bytes.NewBuffer(rsp.BodyBytes()))
}

func (rsp *ProxyResponse) RepeatableWrite(w io.Writer) error {
	defer rsp.resetBodyReader()
	return rsp.Write(w)
}

func (rsp *ProxyResponse) BodyBytes() []byte {
	return DuplicateBytes(rsp.bodyBytes)
}

func (rsp *ProxyResponse) SetBodyBytes(bs []byte) {
	rsp.bodyBytes = bs
	rsp.resetBodyReader()
	rsp.Header.Set("Content-Length", strconv.Itoa(len(bs)))
}

func (rsp *ProxyResponse) Clone() *ProxyResponse {
	buf := bytes.NewBuffer(make([]byte, 0))
	rsp.RepeatableWrite(buf)
	newRsp, err := ProxyResponseFromBytes(buf.Bytes())
	if err != nil {
		panic(err)
	}
	return newRsp
}

func (rsp *ProxyResponse) DeepClone() *ProxyResponse {
	newRsp := rsp.Clone()
	newRsp.DbId = rsp.DbId
	if rsp.Unmangled != nil {
		newRsp.Unmangled = rsp.Unmangled.DeepClone()
	}
	return newRsp
}

func (rsp *ProxyResponse) Eq(other *ProxyResponse) bool {
	if rsp.StatusLine() != other.StatusLine() ||
		!reflect.DeepEqual(rsp.Header, other.Header) ||
		bytes.Compare(rsp.BodyBytes(), other.BodyBytes()) != 0 {
		return false
	}
	return true
}

func (rsp *ProxyResponse) FullMessage() []byte {
	buf := bytes.NewBuffer(make([]byte, 0))
	rsp.RepeatableWrite(buf)
	return buf.Bytes()
}

func (rsp *ProxyResponse) HTTPStatus() string {
	// The status text to be used in the http request
	text := rsp.Status
	if text == "" {
		text = http.StatusText(rsp.StatusCode)
		if text == "" {
			text = "status code " + strconv.Itoa(rsp.StatusCode)
		}
	} else {
		// Just to reduce stutter, if user set rsp.Status to "200 OK" and StatusCode to 200.
		// Not important.
		text = strings.TrimPrefix(text, strconv.Itoa(rsp.StatusCode)+" ")
	}
	return text
}

func (rsp *ProxyResponse) StatusLine() string {
	// Status line, stolen from net/http/response.go
	return fmt.Sprintf("HTTP/%d.%d %03d %s", rsp.ProtoMajor, rsp.ProtoMinor, rsp.StatusCode, rsp.HTTPStatus())
}

func (rsp *ProxyResponse) HeaderSection() string {
	retStr := rsp.StatusLine()
	retStr += "\r\n"
	for k, vs := range rsp.Header {
		for _, v := range vs {
			retStr += fmt.Sprintf("%s: %s\r\n", k, v)
		}
	}
	return retStr
}
func (msg *ProxyWSMessage) String() string {
	var dirStr string
	if msg.Direction == ToClient {
		dirStr = "ToClient"
	} else {
		dirStr = "ToServer"
	}
	return fmt.Sprintf("{WS Message  msg=\"%s\", type=%d, dir=%s}", string(msg.Message), msg.Type, dirStr)
}

func (msg *ProxyWSMessage) Clone() *ProxyWSMessage {
	var retMsg ProxyWSMessage
	retMsg.Type = msg.Type
	retMsg.Message = msg.Message
	retMsg.Direction = msg.Direction
	retMsg.Timestamp = msg.Timestamp
	retMsg.Request = msg.Request
	return &retMsg
}

func (msg *ProxyWSMessage) DeepClone() *ProxyWSMessage {
	retMsg := msg.Clone()
	retMsg.DbId = msg.DbId
	if msg.Unmangled != nil {
		retMsg.Unmangled = msg.Unmangled.DeepClone()
	}
	return retMsg
}

func (msg *ProxyWSMessage) Eq(other *ProxyWSMessage) bool {
	if msg.Type != other.Type ||
		msg.Direction != other.Direction ||
		bytes.Compare(msg.Message, other.Message) != 0 {
		return false
	}
	return true
}

func CopyHeader(hd http.Header) http.Header {
	var ret http.Header = make(http.Header)
	for k, vs := range hd {
		for _, v := range vs {
			ret.Add(k, v)
		}
	}
	return ret
}

func submitRequest(req *ProxyRequest, useProxy bool, proxyHost string,
	proxyPort int, proxyCreds *ProxyCredentials, proxyIsSOCKS bool) error {
	var dialer NetDialer = req.NetDial
	if dialer == nil {
		dialer = net.Dial
	}

	var conn net.Conn
	var err error
	var proxyFormat bool = false
	if useProxy {
		if proxyIsSOCKS {
			var socksCreds *proxy.Auth
			if proxyCreds != nil {
				socksCreds = &proxy.Auth{
					User:     proxyCreds.Username,
					Password: proxyCreds.Password,
				}
			}
			socksDialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("%s:%d", proxyHost, proxyPort), socksCreds, proxy.Direct)
			if err != nil {
				return fmt.Errorf("error creating SOCKS dialer: %s", err.Error())
			}
			conn, err = socksDialer.Dial("tcp", fmt.Sprintf("%s:%d", req.DestHost, req.DestPort))
			if err != nil {
				return fmt.Errorf("error dialing host: %s", err.Error())
			}
			defer conn.Close()
		} else {
			conn, err = dialer("tcp", fmt.Sprintf("%s:%d", proxyHost, proxyPort))
			if err != nil {
				return fmt.Errorf("error dialing proxy: %s", err.Error())
			}
			defer conn.Close()
			if req.DestUseTLS {
				if err := PerformConnect(conn, req.DestHost, req.DestPort); err != nil {
					return err
				}
				proxyFormat = false
			} else {
				proxyFormat = true
			}
		}
	} else {
		conn, err = dialer("tcp", fmt.Sprintf("%s:%d", req.DestHost, req.DestPort))
		if err != nil {
			return fmt.Errorf("error dialing host: %s", err.Error())
		}
		defer conn.Close()
	}

	if req.DestUseTLS {
		tls_conn := tls.Client(conn, &tls.Config{
			InsecureSkipVerify: true,
		})
		conn = tls_conn
	}

	if proxyFormat {
		return req.SubmitProxy(conn, proxyCreds)
	} else {
		return req.Submit(conn)
	}
}

func SubmitRequest(req *ProxyRequest) error {
	return submitRequest(req, false, "", 0, nil, false)
}

func SubmitRequestProxy(req *ProxyRequest, proxyHost string, proxyPort int, creds *ProxyCredentials) error {
	return submitRequest(req, true, proxyHost, proxyPort, creds, false)
}

func SubmitRequestSOCKSProxy(req *ProxyRequest, proxyHost string, proxyPort int, creds *ProxyCredentials) error {
	return submitRequest(req, true, proxyHost, proxyPort, creds, true)
}
