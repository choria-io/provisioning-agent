package host

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/choria-io/go-choria/protocol"
	"github.com/choria-io/go-choria/providers/agent/mcorpc"
	rpc "github.com/choria-io/go-choria/providers/agent/mcorpc/client"
	addl "github.com/choria-io/go-choria/providers/agent/mcorpc/ddl/agent"
	"github.com/choria-io/go-choria/providers/agent/mcorpc/golang/provision"
	"github.com/prometheus/client_golang/prometheus"
)

func (h *Host) rpcDo(ctx context.Context, agent string, action string, input interface{}, cb rpc.Handler) (*rpc.Stats, error) {
	name := fmt.Sprintf("%s#%s", agent, action)

	obs := prometheus.NewTimer(rpcDuration.WithLabelValues(h.cfg.Site, name))
	defer obs.ObserveDuration()

	if h.cfg.Paused() {
		return nil, fmt.Errorf("Provisioning is paused, cannot perform %s", name)
	}

	ddl, err := addl.CachedDDL("choria_provision")
	if err != nil {
		return nil, fmt.Errorf("could not find DDL for agent choria_provision in the agent cache")
	}

	prov, err := rpc.New(h.fw, agent, rpc.DDL(ddl))
	if err != nil {
		rpcErrCtr.WithLabelValues(h.cfg.Site, name).Inc()
		return nil, fmt.Errorf("could not create %s client: %s", agent, err)
	}

	handler := func(pr protocol.Reply, reply *rpc.RPCReply) {
		h.replylock.Lock()
		defer h.replylock.Unlock()

		if reply.Statuscode != mcorpc.OK {
			rpcErrCtr.WithLabelValues(h.cfg.Site, name).Inc()
			h.log.Errorf("Failed reply from %s: %s", pr.SenderID(), reply.Statusmsg)
			return
		}

		if pr.SenderID() == h.Identity {
			cb(pr, reply)
		}
	}

	result, err := prov.Do(ctx, action, input, rpc.Targets([]string{h.Identity}), rpc.Collective("provisioning"), rpc.ReplyHandler(handler), rpc.Workers(1))
	if err != nil {
		rpcErrCtr.WithLabelValues(h.cfg.Site, name).Inc()
		return nil, fmt.Errorf("could not perform %s#%s: %s", agent, action, err)
	}

	if result.Stats().ResponsesCount() != 1 {
		rpcErrCtr.WithLabelValues(h.cfg.Site, name).Inc()
		return nil, fmt.Errorf("could not perform %s#%s: received %d responses while expecting a response from %s", agent, action, result.Stats().ResponsesCount(), h.Identity)
	}

	return result.Stats(), nil

}

func (h *Host) restart(ctx context.Context) error {
	h.log.Info("Restarting node")

	creq := &provision.RestartRequest{
		Token: h.token,
		Splay: 1,
	}

	_, err := h.rpcDo(ctx, "choria_provision", "restart", creq, func(pr protocol.Reply, reply *rpc.RPCReply) {
		r := &provision.Reply{}
		err := json.Unmarshal(reply.Data, r)
		if err != nil {
			h.log.Errorf("Could not parse reply from %s: %s", pr.SenderID(), err)
			return
		}

		h.log.Infof("Restart response: %s", r.Message)
	})

	return err
}

func (h *Host) configure(ctx context.Context) error {
	if len(h.config) == 0 {
		return fmt.Errorf("empty configuration")
	}

	h.log.Info("Configuring node")

	cj, err := json.Marshal(h.config)
	if err != nil {
		return fmt.Errorf("could not encode configuration: %s", err)
	}

	creq := &provision.ConfigureRequest{
		Token:         h.token,
		CA:            h.ca,
		Certificate:   h.cert,
		Configuration: string(cj),
	}

	if h.CSR != nil {
		creq.SSLDir = h.CSR.SSLDir
	}

	_, err = h.rpcDo(ctx, "choria_provision", "configure", creq, func(pr protocol.Reply, reply *rpc.RPCReply) {
		r := &provision.Reply{}
		err := json.Unmarshal(reply.Data, r)
		if err != nil {
			h.log.Errorf("Could not parse reply from %s: %s", pr.SenderID(), err)
			return
		}

		h.log.Infof("Configuration response: %s", r.Message)
	})

	return err
}

func (h *Host) fetchJWT(ctx context.Context) (err error) {
	if h.rawJWT != "" {
		h.log.Infof("Already have JWT for %s, not retrieving again", h.Identity)
		return nil
	}

	h.log.Info("Fetching JWT")

	jwtreq := &provision.JWTRequest{
		Token: h.token,
	}

	for try := 1; try <= 5; try++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		_, err = h.rpcDo(ctx, "choria_provision", "jwt", jwtreq, func(pr protocol.Reply, reply *rpc.RPCReply) {
			resp := &provision.JWTReply{}
			err := json.Unmarshal(reply.Data, resp)
			if err != nil {
				h.log.Errorf("Invalid JSON data: %s", err)
				return
			}

			h.rawJWT = resp.JWT
		})
		if err == nil {
			if len(h.rawJWT) == 0 {
				return fmt.Errorf("received an empty JWT")
			}

			return nil
		}
	}

	return err
}

func (h *Host) fetchInventory(ctx context.Context) (err error) {
	if len(h.Metadata) > 0 {
		h.log.Infof("Already have metadata for %s, not retrieving again", h.Identity)
		return nil
	}

	h.log.Info("Fetching Inventory")

	for try := 1; try <= 5; try++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if try > 1 {
			h.log.Warnf("Could not fetch rpcutil#inventory from %s on try %d / 5, retrying", h.Identity, try-1)
		}

		_, err = h.rpcDo(ctx, "rpcutil", "inventory", struct{}{}, func(pr protocol.Reply, reply *rpc.RPCReply) {
			h.Metadata = string(reply.Data)
		})
		if err == nil {
			return nil
		}
	}

	return err
}

func (h *Host) fetchCSR(ctx context.Context) error {
	h.log.Info("Fetching CSR")

	csreq := &provision.CSRRequest{
		Token: h.token,
		CN:    h.Identity,
	}

	_, err := h.rpcDo(ctx, "choria_provision", "gencsr", csreq, func(pr protocol.Reply, reply *rpc.RPCReply) {
		h.CSR = &provision.CSRReply{}
		err := json.Unmarshal(reply.Data, h.CSR)
		if err != nil {
			h.log.Errorf("Could not parse reply from %s: %s", pr.SenderID(), err)
			return
		}
	})

	return err
}
