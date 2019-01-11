package privval

import (
	"net"
	"sync"
	"time"

	cmn "github.com/tendermint/tendermint/libs/common"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/types"
)

// IPCValOption sets an optional parameter on the SocketPV.
type IPCValOption func(*IPCVal)

// IPCValConnTimeout sets the read and write timeout for connections
// from external signing processes.
func IPCValConnTimeout(timeout time.Duration) IPCValOption {
	return func(sc *IPCVal) { sc.connTimeout = timeout }
}

// IPCValHeartbeat sets the period on which to check the liveness of the
// connected Signer connections.
func IPCValHeartbeat(period time.Duration) IPCValOption {
	return func(sc *IPCVal) { sc.connHeartbeat = period }
}

// IPCVal implements PrivValidator.
// It dials an external process and uses the unencrypted socket
// to request signatures.
type IPCVal struct {
	cmn.BaseService
	*RemoteSignerClient

	addr string

	connTimeout   time.Duration
	connHeartbeat time.Duration

	conn net.Conn
	// connMtx guards writing and reading the field (methods on net.Conn itself are gorountine safe though)
	connMtx sync.RWMutex

	cancelPing chan struct{}
	pingTicker *time.Ticker
}

// Check that IPCVal implements PrivValidator.
var _ types.PrivValidator = (*IPCVal)(nil)

// NewIPCVal returns an instance of IPCVal.
func NewIPCVal(
	logger log.Logger,
	socketAddr string,
) *IPCVal {
	sc := &IPCVal{
		addr:          socketAddr,
		connTimeout:   connTimeout,
		connHeartbeat: connHeartbeat,
	}

	sc.BaseService = *cmn.NewBaseService(logger, "IPCVal", sc)

	return sc
}

// OnStart implements cmn.Service.
func (sc *IPCVal) OnStart() error {
	err := sc.connect()
	if err != nil {
		sc.Logger.Error("OnStart", "err", err)
		return err
	}

	sc.connMtx.RLock()
	defer sc.connMtx.RUnlock()
	sc.RemoteSignerClient, err = NewRemoteSignerClient(sc.conn)
	if err != nil {
		return err
	}

	// Start a routine to keep the connection alive
	sc.cancelPing = make(chan struct{}, 1)
	sc.pingTicker = time.NewTicker(sc.connHeartbeat)
	go func() {
		for {
			select {
			case <-sc.pingTicker.C:
				err := sc.Ping()
				if err != nil {
					sc.Logger.Error(
						"Ping",
						"err",
						err,
					)
					if err == ErrUnexpectedResponse {
						return
					}

					err := sc.connect()
					if err != nil {
						sc.Logger.Error(
							"Reconnecting to remote signer failed",
							"err",
							err,
						)
						continue
					}
					sc.connMtx.RLock()
					sc.RemoteSignerClient, err = NewRemoteSignerClient(sc.conn)
					sc.connMtx.RUnlock()
					if err != nil {
						sc.Logger.Error(
							"Re-initializing remote signer client failed",
							"err",
							err,
						)
						sc.connMtx.RLock()
						if err := sc.conn.Close(); err != nil {
							sc.Logger.Error(
								"error closing connection",
								"err",
								err,
							)
						}
						sc.connMtx.RUnlock()
						continue
					}
					sc.Logger.Info("Re-created connection to remote signer", "impl", sc)
				}
			case <-sc.cancelPing:
				sc.pingTicker.Stop()
				return
			}
		}
	}()

	return nil
}

// OnStop implements cmn.Service.
func (sc *IPCVal) OnStop() {
	if sc.cancelPing != nil {
		close(sc.cancelPing)
	}
	sc.connMtx.RLock()
	defer sc.connMtx.RUnlock()
	if sc.conn != nil {
		if err := sc.conn.Close(); err != nil {
			sc.Logger.Error("OnStop", "err", err)
		}
	}
}

func (sc *IPCVal) connect() error {
	la, err := net.ResolveUnixAddr("unix", sc.addr)
	if err != nil {
		return err
	}

	conn, err := net.DialUnix("unix", nil, la)
	if err != nil {
		return err
	}

	sc.connMtx.Lock()
	defer sc.connMtx.Unlock()
	sc.conn = newTimeoutConn(conn, sc.connTimeout)

	return nil
}
