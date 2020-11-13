package main

import (
	"bytes"
	"fmt"
	"log"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/not.go"
	"github.com/opentracing/opentracing-go"
	"github.com/opentracing/opentracing-go/ext"
	jaegerlog "github.com/opentracing/opentracing-go/log"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
)

type Actor struct{}

func (a Actor) Act() error {
	nc, err := nats.Connect("nats://nats:4222")
	if err != nil {
		return err
	}
	defer nc.Close()

	ch := make(chan *nats.Msg)
	sub, err := nc.ChanSubscribe("job-queue", ch)
	if err != nil {
		return err
	}
	defer sub.Unsubscribe()

	for msg := range ch {
		func() {
			t := &not.TraceMsg{Buffer: *bytes.NewBuffer(msg.Data)}

			// Extract the span context from the request message.
			sc, err := opentracing.GlobalTracer().Extract(opentracing.Binary, t)
			if err != nil {
				log.Printf("Extract error: %v", err)
			}
			span := opentracing.GlobalTracer().StartSpan("worker-response", ext.SpanKindRPCServer, ext.RPCServerOption(sc))
			defer span.Finish()

			span.LogFields(jaegerlog.Object("request", *msg))
			span.LogFields(jaegerlog.Object("request.data", t.String()))

			{ // SIMULATE WORK
				for i := 1; i <= 10; i++ {
					time.Sleep(time.Millisecond)
					span.LogFields(jaegerlog.String("progress", fmt.Sprintf("%d%%", 10*i)))
				}
			}

			if err := msg.Respond([]byte("OK")); err != nil {
				span.SetTag("error", true)
				span.LogFields(jaegerlog.Error(err))
			}
		}()
	}

	return nil
}

func main() {
	cfg := &jaegercfg.Configuration{
		ServiceName: "worker",
		Sampler: &jaegercfg.SamplerConfig{
			Type:  "const",
			Param: 1,
		},
		Reporter: &jaegercfg.ReporterConfig{
			LogSpans: true,
		},
	}
	transport, err := jaeger.NewUDPTransport("jaeger:5775", 0)
	if err != nil {
		log.Printf("Could not initialize jaeger transport: %s", err.Error())
		return
	}

	reporter := jaeger.NewRemoteReporter(transport)
	defer reporter.Close()

	tracer, closer, err := cfg.NewTracer(
		jaegercfg.Logger(jaeger.StdLogger),
		jaegercfg.Reporter(reporter),
	)
	if err != nil {
		log.Printf("Could not initialize jaeger tracer: %s", err.Error())
		return
	}
	defer closer.Close()

	opentracing.SetGlobalTracer(tracer)

	var a Actor
	if err := a.Act(); err != nil {
		log.Fatal(err)
	}
}
