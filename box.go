package box

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/experimental"
	"github.com/sagernet/sing-box/experimental/libbox/platform"
	"github.com/sagernet/sing-box/inbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/outbound"
	"github.com/sagernet/sing-box/proxyprovider"
	"github.com/sagernet/sing-box/route"
	"github.com/sagernet/sing-box/script"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
)

var _ adapter.Service = (*Box)(nil)

type Box struct {
	createdAt    time.Time
	router       adapter.Router
	inbounds     []adapter.Inbound
	outbounds    []adapter.Outbound
	logFactory   log.Factory
	logger       log.ContextLogger
	preServices  map[string]adapter.Service
	postServices map[string]adapter.Service
	scripts      []*script.ScriptService
	done         chan struct{}
}

type Options struct {
	option.Options
	Context           context.Context
	PlatformInterface platform.Interface
}

func New(options Options) (*Box, error) {
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	createdAt := time.Now()
	experimentalOptions := common.PtrValueOrDefault(options.Experimental)
	applyDebugOptions(common.PtrValueOrDefault(experimentalOptions.Debug))
	var needClashAPI bool
	var needV2RayAPI bool
	if experimentalOptions.ClashAPI != nil && experimentalOptions.ClashAPI.ExternalController != "" {
		needClashAPI = true
	}
	if experimentalOptions.V2RayAPI != nil && experimentalOptions.V2RayAPI.Listen != "" {
		needV2RayAPI = true
	}
	var defaultLogWriter io.Writer
	if options.PlatformInterface != nil {
		defaultLogWriter = io.Discard
	}
	logFactory, err := log.New(log.Options{
		Context:        ctx,
		Options:        common.PtrValueOrDefault(options.Log),
		Observable:     needClashAPI,
		DefaultWriter:  defaultLogWriter,
		BaseTime:       createdAt,
		PlatformWriter: options.PlatformInterface,
	})
	if err != nil {
		return nil, E.Cause(err, "create log factory")
	}
	logger := logFactory.Logger()
	router, err := route.NewRouter(
		ctx,
		logFactory,
		common.PtrValueOrDefault(options.Route),
		common.PtrValueOrDefault(options.DNS),
		common.PtrValueOrDefault(options.NTP),
		options.Inbounds,
		options.PlatformInterface,
	)
	if err != nil {
		return nil, E.Cause(err, "parse route options")
	}
	inbounds := make([]adapter.Inbound, 0, len(options.Inbounds))
	outbounds := make([]adapter.Outbound, 0, len(options.Outbounds))
	for i, inboundOptions := range options.Inbounds {
		var in adapter.Inbound
		var tag string
		if inboundOptions.Tag != "" {
			tag = inboundOptions.Tag
		} else {
			tag = F.ToString(i)
		}
		in, err = inbound.New(
			ctx,
			router,
			logFactory.NewLogger(F.ToString("inbound/", inboundOptions.Type, "[", tag, "]")),
			inboundOptions,
			options.PlatformInterface,
		)
		if err != nil {
			return nil, E.Cause(err, "parse inbound[", i, "]")
		}
		inbounds = append(inbounds, in)
	}
	for i, outboundOptions := range options.Outbounds {
		var out adapter.Outbound
		var tag string
		if outboundOptions.Tag != "" {
			tag = outboundOptions.Tag
		} else {
			tag = F.ToString(i)
		}
		out, err = outbound.New(
			ctx,
			router,
			logFactory.NewLogger(F.ToString("outbound/", outboundOptions.Type, "[", tag, "]")),
			tag,
			outboundOptions)
		if err != nil {
			return nil, E.Cause(err, "parse outbound[", i, "]")
		}
		outbounds = append(outbounds, out)
	}
	var proxyProviders []adapter.ProxyProvider
	var proxyProviderOutbounds map[string][]adapter.Outbound
	if options.ProxyProviders != nil && len(options.ProxyProviders) > 0 {
		proxyProviders = make([]adapter.ProxyProvider, 0)
		proxyProviderOutbounds = make(map[string][]adapter.Outbound)
		for i, proxyProviderOptions := range options.ProxyProviders {
			pp, err := proxyprovider.NewProxyProvider(ctx, router, logFactory, proxyProviderOptions)
			if err != nil {
				return nil, E.Cause(err, "parse proxy provider[", i, "]")
			}
			logger.Info("init proxy provider[", i, "]")
			err = pp.Update()
			if err != nil {
				return nil, E.Cause(err, "update proxy provider[", i, "]")
			}
			outs, err := pp.GetOutbounds()
			if err != nil {
				return nil, E.Cause(err, "get outbounds from proxy provider[", i, "]")
			}
			outbounds = append(outbounds, outs...)
			proxyProviderOutbounds[pp.Tag()] = outs
			proxyProviders = append(proxyProviders, pp)
			logger.Info("init proxy provider[", i, "]", " done")
		}
	}
	err = router.Initialize(inbounds, outbounds, proxyProviders, proxyProviderOutbounds, func() adapter.Outbound {
		out, oErr := outbound.New(ctx, router, logFactory.NewLogger("outbound/direct"), "direct", option.Outbound{Type: "direct", Tag: "default"})
		common.Must(oErr)
		outbounds = append(outbounds, out)
		return out
	})
	if err != nil {
		return nil, err
	}
	if options.PlatformInterface != nil {
		err = options.PlatformInterface.Initialize(ctx, router)
		if err != nil {
			return nil, E.Cause(err, "initialize platform interface")
		}
	}
	preServices := make(map[string]adapter.Service)
	postServices := make(map[string]adapter.Service)
	if needClashAPI {
		clashServer, err := experimental.NewClashServer(ctx, router, logFactory.(log.ObservableFactory), common.PtrValueOrDefault(options.Experimental.ClashAPI))
		if err != nil {
			return nil, E.Cause(err, "create clash api server")
		}
		router.SetClashServer(clashServer)
		preServices["clash api"] = clashServer
	}
	if needV2RayAPI {
		v2rayServer, err := experimental.NewV2RayServer(logFactory.NewLogger("v2ray-api"), common.PtrValueOrDefault(options.Experimental.V2RayAPI))
		if err != nil {
			return nil, E.Cause(err, "create v2ray api server")
		}
		router.SetV2RayServer(v2rayServer)
		preServices["v2ray api"] = v2rayServer
	}

	var scripts []*script.ScriptService

	if options.Script != nil && len(options.Script) > 0 {
		scripts = make([]*script.ScriptService, 0)
		for i, s := range options.Script {
			var tag string
			if s.Tag != "" {
				tag = s.Tag
			} else {
				tag = F.ToString(i)
			}
			service := script.NewScript(ctx, logFactory.NewLogger(F.ToString("script", "[", tag, "]")), s)
			scripts = append(scripts, service)
		}
	}

	return &Box{
		router:       router,
		inbounds:     inbounds,
		outbounds:    outbounds,
		createdAt:    createdAt,
		logFactory:   logFactory,
		logger:       logger,
		preServices:  preServices,
		postServices: postServices,
		scripts:      scripts,
		done:         make(chan struct{}),
	}, nil
}

func (s *Box) PreStart() error {
	err := s.preStart()
	if err != nil {
		// TODO: remove catch error
		defer func() {
			v := recover()
			if v != nil {
				log.Error(E.Cause(err, "origin error"))
				debug.PrintStack()
				panic("panic on early close: " + fmt.Sprint(v))
			}
		}()
		s.Close()
		return err
	}
	s.logger.Info("sing-box pre-started (", F.Seconds(time.Since(s.createdAt).Seconds()), "s)")
	return nil
}

func (s *Box) Start() error {
	err := s.start()
	if err != nil {
		// TODO: remove catch error
		defer func() {
			v := recover()
			if v != nil {
				log.Error(E.Cause(err, "origin error"))
				debug.PrintStack()
				panic("panic on early close: " + fmt.Sprint(v))
			}
		}()
		s.Close()
		return err
	}
	s.logger.Info("sing-box started (", F.Seconds(time.Since(s.createdAt).Seconds()), "s)")
	return nil
}

func (s *Box) preStart() error {
	for _, service := range s.scripts {
		if service.GetMode() == "start-pre" {
			if service.GetKeep() {
				err := service.Start()
				if err != nil {
					return E.Cause(err, "run script", "[", service.GetTag(), "]")
				}
				continue
			}
			err := service.RunWithGlobalContext()
			if err != nil {
				return E.Cause(err, "run script", "[", service.GetTag(), "]")
			}
		}
	}

	for serviceName, service := range s.preServices {
		s.logger.Trace("pre-start ", serviceName)
		err := adapter.PreStart(service)
		if err != nil {
			return E.Cause(err, "pre-starting ", serviceName)
		}
	}
	for i, out := range s.outbounds {
		var tag string
		if out.Tag() == "" {
			tag = F.ToString(i)
		} else {
			tag = out.Tag()
		}
		if starter, isStarter := out.(common.Starter); isStarter {
			s.logger.Trace("initializing outbound/", out.Type(), "[", tag, "]")
			err := starter.Start()
			if err != nil {
				return E.Cause(err, "initialize outbound/", out.Type(), "[", tag, "]")
			}
		}
	}
	return s.router.Start()
}

func (s *Box) start() error {
	err := s.preStart()
	if err != nil {
		return err
	}
	for serviceName, service := range s.preServices {
		s.logger.Trace("starting ", serviceName)
		err = service.Start()
		if err != nil {
			return E.Cause(err, "start ", serviceName)
		}
	}
	for i, in := range s.inbounds {
		var tag string
		if in.Tag() == "" {
			tag = F.ToString(i)
		} else {
			tag = in.Tag()
		}
		s.logger.Trace("initializing inbound/", in.Type(), "[", tag, "]")
		err = in.Start()
		if err != nil {
			return E.Cause(err, "initialize inbound/", in.Type(), "[", tag, "]")
		}
	}
	for serviceName, service := range s.postServices {
		s.logger.Trace("starting ", service)
		err = service.Start()
		if err != nil {
			return E.Cause(err, "start ", serviceName)
		}
	}

	for _, service := range s.scripts {
		if service.GetMode() == "start-post" {
			s.logger.Trace("run script", "[", service.GetTag(), "]")
			if service.GetKeep() {
				err := service.Start()
				if err != nil {
					return E.Cause(err, "run script", "[", service.GetTag(), "]")
				}
				continue
			}
			err := service.RunWithGlobalContext()
			if err != nil {
				return E.Cause(err, "run script", "[", service.GetTag(), "]")
			}
		}
	}

	return nil
}

func (s *Box) Close() error {
	select {
	case <-s.done:
		return os.ErrClosed
	default:
		close(s.done)
	}
	var errors error

	for _, service := range s.scripts {
		if service.GetMode() == "close-pre" {
			if service.GetKeep() {
				errors = E.Append(errors, service.Close(), func(err error) error {
					return E.Cause(err, "stop script", "[", service.GetTag(), "]")
				})
				continue
			}
			err := service.RunWithGlobalContext()
			if err != nil {
				errors = E.Append(errors, err, func(err error) error {
					return E.Cause(err, "run script", "[", service.GetTag(), "]")
				})
			}
		}
	}

	for serviceName, service := range s.postServices {
		s.logger.Trace("closing ", serviceName)
		errors = E.Append(errors, service.Close(), func(err error) error {
			return E.Cause(err, "close ", serviceName)
		})
	}
	for i, in := range s.inbounds {
		s.logger.Trace("closing inbound/", in.Type(), "[", i, "]")
		errors = E.Append(errors, in.Close(), func(err error) error {
			return E.Cause(err, "close inbound/", in.Type(), "[", i, "]")
		})
	}
	for i, out := range s.outbounds {
		s.logger.Trace("closing outbound/", out.Type(), "[", i, "]")
		errors = E.Append(errors, common.Close(out), func(err error) error {
			return E.Cause(err, "close outbound/", out.Type(), "[", i, "]")
		})
	}
	s.logger.Trace("closing router")
	if err := common.Close(s.router); err != nil {
		errors = E.Append(errors, err, func(err error) error {
			return E.Cause(err, "close router")
		})
	}
	for serviceName, service := range s.preServices {
		s.logger.Trace("closing ", serviceName)
		errors = E.Append(errors, service.Close(), func(err error) error {
			return E.Cause(err, "close ", serviceName)
		})
	}

	for _, service := range s.scripts {
		if service.GetMode() == "close-post" {
			if service.GetKeep() {
				errors = E.Append(errors, service.Close(), func(err error) error {
					return E.Cause(err, "stop script", "[", service.GetTag(), "]")
				})
				continue
			}
			err := service.RunWithContext(context.Background())
			if err != nil {
				errors = E.Append(errors, err, func(err error) error {
					return E.Cause(err, "run script", "[", service.GetTag(), "]")
				})
			}
		}
	}

	s.logger.Trace("closing log factory")
	if err := common.Close(s.logFactory); err != nil {
		errors = E.Append(errors, err, func(err error) error {
			return E.Cause(err, "close log factory")
		})
	}
	return errors
}

func (s *Box) Router() adapter.Router {
	return s.router
}
