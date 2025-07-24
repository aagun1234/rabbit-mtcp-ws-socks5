package server

import (
	"crypto/tls"

	"github.com/aagun1234/rabbit-mtcp-ws-socks5/logger"
	"github.com/aagun1234/rabbit-mtcp-ws-socks5/peer"
	"github.com/aagun1234/rabbit-mtcp-ws-socks5/tunnel"

	//"crypto/cipher"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Server struct {
	peerGroup peer.PeerGroup
	logger    *logger.Logger
	keyfile   string
	certfile  string
	authkey   string
}

// GetPeerGroup 返回服务器的 PeerGroup
func (s *Server) GetPeerGroup() *peer.PeerGroup {
	return &s.peerGroup
}

func NewServer(cipher tunnel.Cipher, authkey, keyfile, certfile string) Server {
	return Server{
		peerGroup: peer.NewPeerGroup(cipher),
		logger:    logger.NewLogger("[Server]"),
		keyfile:   keyfile,
		certfile:  certfile,
		authkey:   authkey,
	}
}

// func ServeThread1(address string, ss *Server, wg *sync.WaitGroup) error {
// defer wg.Done()
// listener, err := net.Listen("tcp", address)
// if err != nil {
// return err
// }
// defer listener.Close()
// for {
// conn, err := listener.Accept()
// if err != nil {
// ss.logger.Errorf("Error when accept connection: %v.\n", err)
// continue
// }
// err = ss.peerGroup.AddTunnelFromConn(conn)
// if err != nil {
// ss.logger.Errorf("Error when add tunnel to tunnel pool: %v.\n", err)
// }
// }
// }

func ParseWebSocketURL(wsURL string) (schema, hostPort, path string, err error) {
	wsurl := wsURL
	if !strings.HasPrefix(wsurl, "ws://") && !strings.HasPrefix(wsurl, "wss://") {
		if !strings.Contains(wsurl, "://") {
			wsurl = "ws://" + wsurl
		} else {
			return "", "", "", fmt.Errorf("invalid WebSocket URL schema")
		}
	}

	// 使用 url.Parse 解析
	u, err := url.Parse(wsurl)
	if err != nil {
		return "", "", "", fmt.Errorf("failed to parse URL: %v", err)
	}

	// 提取 schema (ws 或 wss)
	schema = u.Scheme

	// 提取 host:port
	hostPort = u.Host
	// 如果端口不存在，添加默认端口
	if !strings.Contains(hostPort, ":") {
		if schema == "ws" {
			hostPort += ":80" // ws 默认端口
		} else if schema == "wss" {
			hostPort += ":443" // wss 默认端口
		}
	}

	// 提取路径 (如果没有路径则返回 "/")
	path = u.Path
	if path == "" {
		path = "/"
	}

	return schema, hostPort, path, nil
}

// TLSConfigFromFiles 从文件加载 TLS 配置
func (s *Server) TLSConfigFromFiles(certFile, keyFile, caFile string, insecureSkipVerify bool) (*tls.Config, error) {
	var tlsConfig tls.Config
	//var err error
	s.logger.Debugf("Loading TLS key pair from %s, %s", certFile, keyFile)
	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("failed to load TLS key pair: %w", err)
		}
		tlsConfig.Certificates = []tls.Certificate{cert}
	}

	if caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA certificate: %w", err)
		}
		caCertPool := x509.NewCertPool()
		if !caCertPool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to append CA certificate to pool")
		}
		tlsConfig.RootCAs = caCertPool                     // For client to verify server
		tlsConfig.ClientCAs = caCertPool                   // For server to verify client (mutual TLS)
		tlsConfig.ClientAuth = tls.VerifyClientCertIfGiven // Or tls.RequireAndVerifyClientCert
	}

	tlsConfig.InsecureSkipVerify = insecureSkipVerify // For client only
	tlsConfig.MinVersion = tls.VersionTLS12           // 推荐最低 TLS 1.2
	return &tlsConfig, nil
}

func (s *Server) ServeThread(wsurl string, wg *sync.WaitGroup) error {
	var goruntimebuf [64]byte
	nn := runtime.Stack(goruntimebuf[:], false)
	goroutineid := "00000000" + strings.Fields(strings.TrimPrefix(string(goruntimebuf[:nn]), "goroutine "))[0]
	goroutineid = goroutineid[len(goroutineid)-8:]

	_, address, wspath, err := ParseWebSocketURL(wsurl)
	if err == nil {
		// 创建HTTP路由器
		mux := http.NewServeMux()
		mux.HandleFunc(wspath, func(w http.ResponseWriter, r *http.Request) {
			//处理认证
			if s.authkey != "" {
				authHeader := r.Header.Get("Authorization")
				expectedAuth := "Bearer " + s.authkey
				if authHeader != expectedAuth {
					s.logger.Errorf("[%s] Unauthorized WebSocket connection attempt from %s (missing or invalid Authorization header)", goroutineid, r.RemoteAddr)
					http.Error(w, "Unauthorized", http.StatusUnauthorized)
					return
				}
			}

			// 升级为WebSocket连接
			upgrader := websocket.Upgrader{
				ReadBufferSize:  4096,
				WriteBufferSize: 4096,
				CheckOrigin: func(r *http.Request) bool {
					return true // 允许所有来源
				},
			}
			s.logger.Infof("Upgrade to WebSocket for %s", r.RemoteAddr)
			wsConn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				s.logger.Errorf("Failed to upgrade to WebSocket for %s: %v", r.RemoteAddr, err)
				return
			}
			err = s.peerGroup.AddTunnelFromConn(wsConn)
			if err != nil {
				s.logger.Errorf("Error when add tunnel to tunnel pool: %v", err)
				wsConn.Close()
			}
		})

		// 启动HTTP服务器
		server := &http.Server{
			Addr:    address,
			Handler: mux,
		}
		var err error
		if s.certfile != "" && s.keyfile != "" {
			s.logger.Debugf("[%s] Config WSS (TLS) on %s", goroutineid, address)
			tlsConfig, err := s.TLSConfigFromFiles(s.certfile, s.keyfile, "", true)
			if err != nil {
				s.logger.Errorf("Failed to create server TLS config: %v", err)
				return err
			}
			server.TLSConfig = tlsConfig
			s.logger.Logf("Listening on WSS %s/tunnel", address)
			return server.ListenAndServeTLS(s.certfile, s.keyfile)
		} else {
			s.logger.Logf("Listening on WS %s/tunnel", address)
			return server.ListenAndServe()
		}

		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.logger.Fatalf("HTTP server ListenAndServe error: %v", err)
		}

		//s.logger.Infof("WebSocket server listening on %s", address)
		return server.ListenAndServe()
	}
	return err
}

func (s *Server) Serve(addresses []string) error {
	var wg sync.WaitGroup

	for _, address := range addresses {
		wg.Add(1)
		go s.ServeThread(address, &wg)
	}
	wg.Wait()
	return nil
}

func getGoroutineID() int {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	idField := strings.Fields(strings.TrimPrefix(string(buf[:n]), "goroutine "))[0]
	id, err := strconv.Atoi(idField)
	if err != nil {
		panic(fmt.Sprintf("cannot get goroutine id: %v", err))
	}
	return id
}

//===================================

type WebsocketConnAdapter struct {
	*websocket.Conn
	reader io.Reader
}

func (c *WebsocketConnAdapter) Read(b []byte) (int, error) {
	// WebSocket消息可能是分帧的，需要处理消息边界
	if c.reader == nil {
		_, r, err := c.Conn.NextReader()
		if err != nil {
			return 0, err
		}
		c.reader = r
	}

	n, err := c.reader.Read(b)
	if err == io.EOF {
		c.reader = nil
		return n, nil
	}
	return n, err
}

func (c *WebsocketConnAdapter) Write(b []byte) (int, error) {
	err := c.Conn.WriteMessage(websocket.BinaryMessage, b)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}

// 确保实现所有net.Conn接口方法
func (c *WebsocketConnAdapter) SetDeadline(t time.Time) error {
	err := c.SetReadDeadline(t)
	if err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

func (c *WebsocketConnAdapter) SetReadDeadline(t time.Time) error {
	return c.Conn.SetReadDeadline(t)
}

func (c *WebsocketConnAdapter) SetWriteDeadline(t time.Time) error {
	return c.Conn.SetWriteDeadline(t)
}
