package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/code-ready/gvisor-tap-vsock/pkg/transport"
	"github.com/code-ready/gvisor-tap-vsock/pkg/types"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/google/tcpproxy"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/songgao/packets/ethernet"
	"github.com/songgao/water"
	"github.com/vishvananda/netlink"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

var (
	endpoint           string
	iface              string
	stopIfIfaceExist   string
	debug              bool
	changeDefaultRoute bool
)

func main() {
	flag.StringVar(&endpoint, "url", fmt.Sprintf("vsock://2:1024%s", types.ConnectPath), "url where the tap send packets")
	flag.StringVar(&iface, "iface", "tap0", "tap interface name")
	flag.StringVar(&stopIfIfaceExist, "stop-if-exist", "eth0,ens3,enp0s1", "stop if one of these interfaces exists at startup")
	flag.BoolVar(&debug, "debug", false, "debug")
	flag.BoolVar(&changeDefaultRoute, "change-default-route", true, "change the default route to use this interface")
	flag.Parse()

	expected := strings.Split(stopIfIfaceExist, ",")
	links, err := netlink.LinkList()
	if err != nil {
		log.Fatal(err)
	}
	for _, link := range links {
		if contains(expected, link.Attrs().Name) {
			log.Infof("interface %s prevented this program to run", link.Attrs().Name)
			return
		}
	}

	if err := exposePodman(); err != nil {
		log.Fatal(err)
	}

	for {
		if err := run(); err != nil {
			log.Error(err)
		}
		time.Sleep(time.Second)
	}
}

func exposePodman() error {
	var p tcpproxy.Proxy
	p.AddRoute(":1234", &tcpproxy.DialProxy{
		DialContext: func(ctx context.Context, network, addr string) (conn net.Conn, e error) {
			return net.Dial("unix", "/var/run/podman/podman.sock")
		},
	})
	if err := p.Start(); err != nil {
		return err
	}
	go func() {
		if err := p.Wait(); err != nil {
			log.Error(err)
		}
	}()
	return nil
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

func run() error {
	conn, path, err := transport.Dial(endpoint)
	if err != nil {
		return errors.Wrap(err, "cannot connect to host")
	}
	defer conn.Close()

	req, err := http.NewRequest("POST", path, nil)
	if err != nil {
		return err
	}
	if err := req.Write(conn); err != nil {
		return err
	}

	handshake, err := handshake(conn)
	if err != nil {
		return errors.Wrap(err, "cannot handshake")
	}

	tap, err := water.New(water.Config{
		DeviceType: water.TAP,
		PlatformSpecificParams: water.PlatformSpecificParams{
			Name: iface,
		},
	})
	if err != nil {
		return errors.Wrap(err, "cannot create tap device")
	}
	defer tap.Close()

	errCh := make(chan error, 1)
	go tx(conn, tap, errCh, handshake.MTU)
	go rx(conn, tap, errCh, handshake.MTU)

	c := make(chan os.Signal)
	cleanup, err := linkUp(handshake)
	defer func() {
		signal.Stop(c)
		cleanup()
	}()
	if err != nil {
		return err
	}
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-c
		cleanup()
		os.Exit(0)
	}()
	return <-errCh
}

func handshake(conn net.Conn) (*types.HandshakeResponse, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	bin, err := json.Marshal(&types.HandshakeRequest{
		Hostname:   hostname,
		DeviceType: deviceType,
	})
	if err != nil {
		return nil, err
	}
	writeSize := make([]byte, 2)
	binary.LittleEndian.PutUint16(writeSize, uint16(len(bin)))

	if _, err := conn.Write(writeSize); err != nil {
		return nil, err
	}
	if _, err := conn.Write(bin); err != nil {
		return nil, err
	}

	sizeBuf := make([]byte, 2)
	if _, err := io.ReadFull(conn, sizeBuf); err != nil {
		return nil, err
	}
	readSize := int(binary.LittleEndian.Uint16(sizeBuf[0:2]))
	b := make([]byte, readSize)
	if _, err := io.ReadFull(conn, b); err != nil {
		return nil, err
	}
	var handshake types.HandshakeResponse
	if err := json.Unmarshal(b, &handshake); err != nil {
		return nil, err
	}
	return &handshake, nil
}

func rx(conn net.Conn, tap *water.Interface, errCh chan error, mtu int) {
	log.Info("waiting for packets...")
	var frame ethernet.Frame
	for {
		frame.Resize(mtu)
		n, err := tap.Read([]byte(frame))
		if err != nil {
			errCh <- errors.Wrap(err, "cannot read packet from tap")
			return
		}
		frame = frame[:n]

		if debug {
			packet := gopacket.NewPacket(frame, layers.LayerTypeEthernet, gopacket.Default)
			log.Info(packet.String())
		}

		size := make([]byte, 2)
		binary.LittleEndian.PutUint16(size, uint16(n))

		if _, err := conn.Write(size); err != nil {
			errCh <- errors.Wrap(err, "cannot write size to socket")
			return
		}
		if _, err := conn.Write(frame); err != nil {
			errCh <- errors.Wrap(err, "cannot write packet to socket")
			return
		}
	}
}

func tx(conn net.Conn, tap *water.Interface, errCh chan error, mtu int) {
	sizeBuf := make([]byte, 2)
	buf := make([]byte, mtu+header.EthernetMinimumSize)

	for {
		n, err := io.ReadFull(conn, sizeBuf)
		if err != nil {
			errCh <- errors.Wrap(err, "cannot read size from socket")
			return
		}
		if n != 2 {
			errCh <- fmt.Errorf("unexpected size %d", n)
			return
		}
		size := int(binary.LittleEndian.Uint16(sizeBuf[0:2]))

		n, err = io.ReadFull(conn, buf[:size])
		if err != nil {
			errCh <- errors.Wrap(err, "cannot read payload from socket")
			return
		}
		if n == 0 || n != size {
			errCh <- fmt.Errorf("unexpected size %d != %d", n, size)
			return
		}

		if debug {
			packet := gopacket.NewPacket(buf[:size], layers.LayerTypeEthernet, gopacket.Default)
			log.Info(packet.String())
		}

		if _, err := tap.Write(buf[:size]); err != nil {
			errCh <- errors.Wrap(err, "cannot write packet to tap")
			return
		}
	}
}
