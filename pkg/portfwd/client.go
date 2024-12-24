package portfwd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/lima-vm/lima/pkg/bicopy"
	"github.com/lima-vm/lima/pkg/guestagent/api"
	guestagentclient "github.com/lima-vm/lima/pkg/guestagent/api/client"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
)

func HandleTCPConnection(ctx context.Context, client *guestagentclient.GuestAgentClient, conn net.Conn, guestAddr string) {
	defer conn.Close()

	id := fmt.Sprintf("tcp-%s-%s", conn.LocalAddr().String(), conn.RemoteAddr().String())

	stream, err := client.Tunnel(ctx)
	if err != nil {
		logrus.Errorf("could not open tcp tunnel for id: %s error:%v", id, err)
		return
	}

	rw := &GrpcClientRW{stream: stream, id: id, addr: guestAddr}

	bicopy.Bicopy("tcp tunnel", bicopy.NamedReadWriter{
		ReadWriter: rw,
		Name:       guestAddr,
	}, bicopy.NamedReadWriter{
		ReadWriter: conn,
		Name:       id,
	})
}

func HandleUDPConnection(ctx context.Context, client *guestagentclient.GuestAgentClient, conn net.PacketConn, guestAddr string) {
	defer conn.Close()

	id := fmt.Sprintf("udp-%s", conn.LocalAddr().String())

	stream, err := client.Tunnel(ctx)
	if err != nil {
		logrus.Errorf("could not open udp tunnel for id: %s error:%v", id, err)
		return
	}

	g, _ := errgroup.WithContext(ctx)

	g.Go(func() error {
		buf := make([]byte, 65507)
		for {
			n, addr, err := conn.ReadFrom(buf)
			// We must handle n > 0 bytes before considering the error.
			// https://pkg.go.dev/net#PacketConn
			if n > 0 {
				msg := &api.TunnelMessage{
					Id:            id + "-" + addr.String(),
					Protocol:      "udp",
					GuestAddr:     guestAddr,
					Data:          buf[:n],
					UdpTargetAddr: addr.String(),
				}
				if err := stream.Send(msg); err != nil {
					return err
				}
			}
			if err != nil {
				// https://pkg.go.dev/net#PacketConn does not mention io.EOF semantics.
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
		}
	})

	g.Go(func() error {
		for {
			// Not documented: when err != nil, in is always nil.
			in, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					return nil
				}
				return err
			}
			addr, err := net.ResolveUDPAddr("udp", in.UdpTargetAddr)
			if err != nil {
				return err
			}
			_, err = conn.WriteTo(in.Data, addr)
			if err != nil {
				return err
			}
		}
	})

	if err := g.Wait(); err != nil {
		logrus.Debugf("error in udp tunnel for id: %s error:%v", id, err)
	}
}

type GrpcClientRW struct {
	id     string
	addr   string
	stream api.GuestService_TunnelClient
}

var _ io.ReadWriter = (*GrpcClientRW)(nil)

func (g GrpcClientRW) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}
	err = g.stream.Send(&api.TunnelMessage{
		Id:        g.id,
		GuestAddr: g.addr,
		Data:      p,
		Protocol:  "tcp",
	})
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (g GrpcClientRW) Read(p []byte) (n int, err error) {
	// Not documented: when err != nil, in is always nil.
	in, err := g.stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil
		}
		return 0, err
	}
	if len(in.Data) == 0 {
		return 0, nil
	}
	copy(p, in.Data)
	return len(in.Data), nil
}
