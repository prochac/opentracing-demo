package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	storagepb "service/storage"
	"time"

	"github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/not.go"
	"github.com/opentracing-contrib/go-stdlib/nethttp"
	"github.com/opentracing/opentracing-go"
	jaegerlog "github.com/opentracing/opentracing-go/log"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
	"google.golang.org/grpc"
)

func failingOperation(ctx context.Context) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "failing operation")
	defer span.Finish()

	err := errors.New("Exemplary error :) Everything is stored correctly, though.")
	if err != nil {
		span.SetTag("error", true)
		span.LogFields(jaegerlog.Error(err))
		return err
	}

	return nil
}

func jobRequest(ctx context.Context, ns *nats.Conn, value string) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "job request")
	defer span.Finish()

	var t not.TraceMsg
	if err := opentracing.GlobalTracer().Inject(span.Context(), opentracing.Binary, &t); err != nil {
		log.Fatalf("%v for Inject.", err)
	}

	if err := json.NewEncoder(&t).Encode(map[string]string{"value": value}); err != nil {
		span.SetTag("error", true)
		span.LogFields(jaegerlog.Error(err))
		return err
	}

	if err := ns.Publish("job-queue", t.Bytes()); err != nil {
		span.SetTag("error", true)
		span.LogFields(jaegerlog.Error(err))
		return err
	}

	return nil
}

func saveToStorage(ctx context.Context, grpcc storagepb.ResourceServiceClient, value string) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "save to storage")
	defer span.Finish()

	resource, err := grpcc.Save(ctx, &storagepb.SaveResourceRequest{Value: value})
	if err != nil {
		span.SetTag("error", true)
		span.LogFields(jaegerlog.Error(err))
		return err
	}

	span.LogFields(jaegerlog.Object("resource", resource))
	return nil
}

func rootHandler(grpcc storagepb.ResourceServiceClient, ns *nats.Conn) http.Handler {
	type request struct {
		Value string `json:"value"`
	}
	f := func(w http.ResponseWriter, r *http.Request) {
		span, ctx := opentracing.StartSpanFromContext(r.Context(), "root-handler")
		defer span.Finish()

		var data request
		if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
			err := fmt.Errorf("decoding input: %v", err)
			span.SetTag("error", true)
			span.LogFields(jaegerlog.Error(err))

			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		span.SetTag("value", data.Value)

		if err := saveToStorage(ctx, grpcc, data.Value); err != nil {
			span.SetTag("error", true)
			span.LogFields(jaegerlog.Error(fmt.Errorf("failed to do some operation: %v", err)))

			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := jobRequest(ctx, ns, data.Value); err != nil {
			span.SetTag("error", true)
			span.LogFields(jaegerlog.Error(fmt.Errorf("failed to do some operation: %v", err)))

			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if err := failingOperation(ctx); err != nil {
			span.SetTag("error", true)
			span.LogFields(jaegerlog.Error(fmt.Errorf("failed to do some operation: %v", err)))

			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "OK")
	}
	return http.HandlerFunc(f)
}

func main() {
	cfg := &jaegercfg.Configuration{
		ServiceName: "service",
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

	nc, err := nats.Connect("nats://nats:4222")
	if err != nil {
		log.Printf("Could not connect to NATS: %s", err.Error())
		return
	}
	defer nc.Close()

	ctx, _ := context.WithTimeout(context.Background(), 5*time.Second)
	conn, err := grpc.DialContext(ctx, "storage:80", grpc.WithInsecure(), grpc.WithBlock(),
		grpc.WithUnaryInterceptor(grpc_opentracing.UnaryClientInterceptor(grpc_opentracing.WithTracer(tracer))),
		grpc.WithStreamInterceptor(grpc_opentracing.StreamClientInterceptor(grpc_opentracing.WithTracer(tracer))),
	)
	if err != nil {
		log.Printf("Could not connect to NATS: %s", err.Error())
		return
	}
	defer conn.Close()

	grpcc := storagepb.NewResourceServiceClient(conn)

	http.Handle("/", nethttp.Middleware(
		tracer,
		rootHandler(grpcc, nc),
		nethttp.OperationNameFunc(func(r *http.Request) string {
			return fmt.Sprintf("%s %s %s", r.Proto, r.Method, r.URL.Path)
		}),
	))

	fmt.Println("starting at http://service.localhost")
	log.Fatal(http.ListenAndServe(":http", nil))
}
