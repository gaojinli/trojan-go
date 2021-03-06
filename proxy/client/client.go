package client

import (
	"context"
	"io"
	"net"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/conf"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/protocol"
	"github.com/p4gefau1t/trojan-go/protocol/direct"
	"github.com/p4gefau1t/trojan-go/protocol/http"
	"github.com/p4gefau1t/trojan-go/protocol/socks"
	"github.com/p4gefau1t/trojan-go/protocol/trojan"
	"github.com/p4gefau1t/trojan-go/proxy"
	"github.com/p4gefau1t/trojan-go/router"
	"github.com/p4gefau1t/trojan-go/stat"
)

type TransportManager interface {
	DialToServer() (io.ReadWriteCloser, error)
}

type packetInfo struct {
	request *protocol.Request
	packet  []byte
}

type Client struct {
	common.Runnable
	proxy.Buildable

	config      *conf.GlobalConfig
	ctx         context.Context
	cancel      context.CancelFunc
	associated  *common.Notifier
	router      router.Router
	tcpListener net.Listener
	udpListener *net.UDPConn
	auth        stat.Authenticator
	appMan      *AppManager
}

func (c *Client) handleSocksConn(conn io.ReadWriteCloser) {
	rwc := common.NewRewindReadWriteCloser(conn)
	inboundConn, req, err := socks.NewInboundConnSession(rwc)
	if err != nil {
		log.Error(common.NewError("failed to handle socks requests").Base(err))
		rwc.Close()
		return
	}
	defer inboundConn.Close()

	if req.Command == protocol.Associate {
		//setting up the bind address to respond
		//listenUDP() will handle the incoming udp packets
		localIP, err := c.config.LocalAddress.ResolveIP(false)
		if err != nil {
			log.Error(common.NewError("invalid local address").Base(err))
			return
		}
		//bind port and IP
		req.IP = localIP
		req.Port = c.config.LocalAddress.Port
		if localIP.To4() != nil {
			req.AddressType = common.IPv4
		} else {
			req.AddressType = common.IPv6
		}

		//notify listenUDP to get ready for relaying udp packets
		c.associated.Signal()
		log.Debug("udp associated to", req)
		if err := inboundConn.(protocol.NeedRespond).Respond(); err != nil {
			log.Error("failed to repsond")
			return
		}

		//stop relaying UDP once TCP connection is closed
		var buf [1]byte
		_, err = rwc.Read(buf[:])
		log.Debug(common.NewError("udp conn ends").Base(err))
		return
	}

	if err := inboundConn.(protocol.NeedRespond).Respond(); err != nil {
		log.Error(common.NewError("failed to respond").Base(err))
		return
	}

	policy, err := c.router.RouteRequest(req)
	if err != nil {
		log.Error(err)
		return
	}
	if policy == router.Bypass {
		outboundConn, err := direct.NewOutboundConnSession(c.ctx, req, c.config)
		if err != nil {
			log.Error(err)
			return
		}
		log.Info("[bypass] conn to", req)
		proxy.ProxyConn(c.ctx, inboundConn, outboundConn, c.config.BufferSize)
		return
	} else if policy == router.Block {
		log.Info("[block] conn to", req)
		return
	}
	outboundConn, err := c.appMan.OpenAppConn(req)
	if err != nil {
		log.Error(err)
		return
	}
	defer outboundConn.Close()
	proxy.ProxyConn(c.ctx, inboundConn, outboundConn, c.config.BufferSize)
}

func (c *Client) handleHTTPConn(conn io.ReadWriteCloser) {
	rwc := common.NewRewindReadWriteCloser(conn)
	inboundConn, req, inboundPacket, err := http.NewHTTPInbound(rwc)
	if err != nil {
		log.Error(common.NewError("failed to handle HTTP requests").Base(err))
		rwc.Close()
		return
	}

	if inboundConn != nil { //CONNECT requests
		defer inboundConn.Close()

		if err := inboundConn.(protocol.NeedRespond).Respond(); err != nil {
			log.Error(common.NewError("failed to respond").Base(err))
			return
		}

		policy, err := c.router.RouteRequest(req)
		if err != nil {
			log.Error(err)
			return
		}
		if policy == router.Bypass {
			outboundConn, err := direct.NewOutboundConnSession(c.ctx, req, c.config)
			if err != nil {
				log.Error(err)
				return
			}
			log.Info("[bypass]conn to", req)
			proxy.ProxyConn(c.ctx, inboundConn, outboundConn, c.config.BufferSize)
			return
		} else if policy == router.Block {
			log.Info("[block]conn to", req)
			return
		}

		outboundConn, err := c.appMan.OpenAppConn(req)
		if err != nil {
			log.Error(common.NewError("fail to start conn session").Base(err))
			return
		}
		defer outboundConn.Close()
		log.Info("conn tunneling to", req)
		proxy.ProxyConn(c.ctx, inboundConn, outboundConn, c.config.BufferSize)
	} else { //GET/POST requests
		defer inboundPacket.Close()
		packetChan := make(chan *packetInfo, 512)
		errChan := make(chan error, 1)

		readHTTPPackets := func() {
			for {
				req, packet, err := inboundPacket.ReadPacket()
				if err != nil {
					log.Error(common.NewError("failed to parse packet").Base(err))
					return
				}
				if req.String() == c.config.LocalAddress.String() { //loop
					err := common.NewError("HTTP loop detected")
					errChan <- err
					log.Error(err)
					return
				}
				if err != nil {
					log.Error(err)
					errChan <- err
					return
				}
				packetChan <- &packetInfo{
					request: req,
					packet:  packet,
				}
			}
		}

		writeHTTPPackets := func() {
			for {
				select {
				case <-errChan:
					return
				case packet := <-packetChan:
					outboundConn, err := c.appMan.OpenAppConn(req)
					if err != nil {
						log.Error(err)
						continue
					}
					_, err = outboundConn.Write(packet.packet)
					if err != nil {
						log.Error(err)
						continue
					}
					go func(outboundConn protocol.ConnSession) {
						buf := [4096]byte{}
						defer outboundConn.Close()
						for {
							n, err := outboundConn.Read(buf[:])
							if err != nil {
								log.Debug(err)
								return
							}
							if _, err = inboundPacket.WritePacket(nil, buf[0:n]); err != nil {
								log.Debug(err)
								return
							}
						}
					}(outboundConn)
				case <-c.ctx.Done():
					return
				}
			}
		}

		go readHTTPPackets()
		writeHTTPPackets()
	}
}

func (c *Client) listenUDP(errChan chan error) {
	localIP, err := c.config.LocalAddress.ResolveIP(false)
	if err != nil {
		errChan <- common.NewError("invalid local address").Base(err)
		return
	}
	listener, err := net.ListenUDP("udp", &net.UDPAddr{
		IP:   localIP,
		Port: c.config.LocalAddress.Port,
	})
	if err != nil {
		errChan <- common.NewError("failed to listen udp").Base(err)
		return
	}
	c.udpListener = listener
	inboundPacket, err := socks.NewInboundPacketSession(c.ctx, listener)
	common.Must(err)
	for {
		select {
		case <-c.associated.Wait():
			log.Debug("associated signal")
			req := &protocol.Request{
				Address: &common.Address{
					DomainName:  "UDP_CONN",
					AddressType: common.DomainName,
				},
				Command: protocol.Associate,
			}
			outboundConn, err := c.appMan.OpenAppConn(req)
			if err != nil {
				log.Error(common.NewError("failed to init udp tunnel").Base(err))
				return
			}
			outboundPacket, err := trojan.NewPacketSession(outboundConn)
			common.Must(err)
			directOutboundPacket, err := direct.NewOutboundPacketSession(c.ctx)
			common.Must(err)
			table := map[router.Policy]protocol.PacketReadWriter{
				router.Proxy:  outboundPacket,
				router.Bypass: directOutboundPacket,
			}
			proxy.ProxyPacketWithRouter(c.ctx, inboundPacket, table, c.router)
			outboundPacket.Close()
			directOutboundPacket.Close()
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) listenTCP(errChan chan error) {
	listener, err := net.Listen("tcp", c.config.LocalAddress.String())
	if err != nil {
		errChan <- common.NewError("failed to listen local address").Base(err)
		return
	}
	c.tcpListener = listener
	defer listener.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			errChan <- common.NewError("error occured when accepting conn").Base(err)
			return
		}
		rwc := common.NewRewindReadWriteCloser(conn)
		rwc.SetBufferSize(128)
		first, err := rwc.ReadByte()
		if err != nil {
			log.Error(common.NewError("failed to obtain proxy type").Base(err))
			rwc.Close()
			continue
		}
		rwc.Rewind()
		rwc.StopBuffering()
		if first == 0x05 {
			go c.handleSocksConn(rwc)
		} else {
			go c.handleHTTPConn(rwc)
		}
	}
}

func (c *Client) Run() error {
	log.Info("client is running at", c.config.LocalAddress.String())
	errChan := make(chan error, 3)
	go c.listenUDP(errChan)
	go c.listenTCP(errChan)
	if c.config.API.Enabled {
		go func() {
			errChan <- proxy.RunAPIService(conf.Client, c.ctx, c.config, c.auth)
		}()
	}
	select {
	case err := <-errChan:
		return err
	case <-c.ctx.Done():
		return nil
	}
}

func (c *Client) Close() error {
	log.Info("shutting down client..")
	c.cancel()
	if c.udpListener != nil {
		c.udpListener.Close()
	}
	if c.tcpListener != nil {
		c.tcpListener.Close()
	}
	return nil
}

func (c *Client) Build(config *conf.GlobalConfig) (common.Runnable, error) {
	ctx, cancel := context.WithCancel(context.Background())
	auth, err := stat.NewAuth(ctx, "memory", config)
	if err != nil {
		cancel()
		return nil, err
	}

	var rtr router.Router = &router.EmptyRouter{}
	if config.Router.Enabled {
		log.Info("router enabled")
		rtr, err = router.NewRouter(&config.Router)
		if err != nil {
			log.Fatal(common.NewError("invalid router list").Base(err))
		}
	}
	appMan := NewAppManager(ctx, config, auth)

	newClient := &Client{
		ctx:        ctx,
		cancel:     cancel,
		config:     config,
		router:     rtr,
		associated: common.NewNotifier(),
		auth:       auth,
		appMan:     appMan,
	}
	return newClient, nil
}

func init() {
	proxy.RegisterProxy(conf.Client, &Client{})
}
