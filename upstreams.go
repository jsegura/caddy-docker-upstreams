package caddy_docker_upstreams

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

const (
	LabelEnable       = "com.caddyserver.http.enable"
	LabelUpstreamPort = "com.caddyserver.http.upstream.port"
)

func init() {
	caddy.RegisterModule(&Upstreams{})
}

type candidate struct {
	matchers caddyhttp.MatcherSet
	upstream *reverseproxy.Upstream
}

// Upstreams provides upstreams from the docker host.
type Upstreams struct {
	logger *zap.Logger

	mu         sync.RWMutex
	candidates []candidate
}

func (u *Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.docker",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

func (u *Upstreams) toCandidates(ctx caddy.Context, containers []types.Container) []candidate {
	candidates := make([]candidate, 0, len(containers))

	for _, container := range containers {
		// Check enable.
		if enable, ok := container.Labels[LabelEnable]; !ok || enable != "true" {
			continue
		}

		// Build matchers.
		var matchers caddyhttp.MatcherSet

		for key, producer := range producers {
			value, ok := container.Labels[key]
			if !ok {
				continue
			}

			matcher := producer(value)
			if prov, ok := matcher.(caddy.Provisioner); ok {
				err := prov.Provision(ctx)
				if err != nil {
					u.logger.Error("unable to provision matcher",
						zap.String("key", key),
						zap.String("value", value),
						zap.Error(err),
					)
					continue
				}
			}
			matchers = append(matchers, matcher)
		}

		// Build upstream.
		port, ok := container.Labels[LabelUpstreamPort]
		if !ok {
			u.logger.Error("unable to get port from container labels", zap.String("container_id", container.ID))
			continue
		}

		if len(container.NetworkSettings.Networks) == 0 {
			u.logger.Error("unable to get ip address from container networks", zap.String("container_id", container.ID))
			continue
		}

		// Use the first network settings of container.
		for _, settings := range container.NetworkSettings.Networks {
			address := net.JoinHostPort(settings.IPAddress, port)
			upstream := &reverseproxy.Upstream{Dial: address}

			candidates = append(candidates, candidate{
				matchers: matchers,
				upstream: upstream,
			})
			break
		}
	}

	return candidates
}

func (u *Upstreams) keepUpdated(ctx caddy.Context, cli *client.Client) {
	for {
		messages, errs := cli.Events(ctx, types.EventsOptions{
			Filters: filters.NewArgs(filters.Arg("type", events.ContainerEventType)),
		})

	selectLoop:
		for {
			select {
			case <-messages:
				containers, err := cli.ContainerList(ctx, types.ContainerListOptions{
					Filters: filters.NewArgs(filters.Arg("label", LabelEnable)),
				})
				if err != nil {
					u.logger.Error("unable to get the list of containers", zap.Error(err))
					continue
				}

				candidates := u.toCandidates(ctx, containers)

				u.mu.Lock()
				u.candidates = candidates
				u.mu.Unlock()
			case err := <-errs:
				if errors.Is(err, context.Canceled) {
					return
				}

				u.logger.Warn("unable to monitor container events; will retry", zap.Error(err))
				break selectLoop
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (u *Upstreams) Provision(ctx caddy.Context) error {
	u.logger = ctx.Logger()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}

	ping, err := cli.Ping(ctx)
	if err != nil {
		return err
	}

	u.logger.Info("docker engine is connected", zap.String("api_version", ping.APIVersion))

	options := types.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelEnable)),
	}
	containers, err := cli.ContainerList(ctx, options)
	if err != nil {
		return err
	}

	u.candidates = u.toCandidates(ctx, containers)

	go u.keepUpdated(ctx, cli)

	return nil
}

func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	upstreams := make([]*reverseproxy.Upstream, 0, 1)

	u.mu.RLock()
	defer u.mu.RUnlock()

	for _, container := range u.candidates {
		if !container.matchers.Match(r) {
			continue
		}

		upstreams = append(upstreams, container.upstream)
	}

	return upstreams, nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
)
