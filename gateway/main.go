package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi"
	"github.com/go-chi/cors"
	"github.com/opentracing-contrib/go-stdlib/nethttp"
	"github.com/opentracing/opentracing-go"
	jaegerlog "github.com/opentracing/opentracing-go/log"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
)

func callService(ctx context.Context, value string) error {
	span, ctx := opentracing.StartSpanFromContext(ctx, "call to service B")
	defer span.Finish()

	body := strings.NewReader(fmt.Sprintf(`{"value":"%s"}`, value))
	req, err := http.NewRequest(http.MethodPost, "http://service:80/resource", body)
	if err != nil {
		err := fmt.Errorf("create request to service: %w", err)
		span.SetTag("error", true)
		span.LogFields(jaegerlog.Error(err))
		return err
	}

	carrier := opentracing.HTTPHeadersCarrier(req.Header)
	if err := opentracing.GlobalTracer().Inject(span.Context(), opentracing.HTTPHeaders, carrier); err != nil {
		err := fmt.Errorf("inject span to request: %w", err)
		span.SetTag("error", true)
		span.LogFields(jaegerlog.Error(err))
		return err
	}

	resp, err := http.DefaultClient.Do(req)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		err := fmt.Errorf("do request to service: %w", err)
		span.SetTag("error", true)
		span.LogFields(jaegerlog.Error(err))
		return err
	}

	span.LogFields(jaegerlog.Int("service-status_code", resp.StatusCode))

	if resp.StatusCode != http.StatusOK {
		b, _ := ioutil.ReadAll(resp.Body)

		err := fmt.Errorf("service response is not 200: %s", b)
		span.SetTag("error", true)
		span.LogFields(jaegerlog.Error(err))
		return err
	}

	return nil
}

func apiHandler() http.HandlerFunc {
	type request struct {
		Value string `json:"value"`
	}
	return func(w http.ResponseWriter, r *http.Request) {
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

		if err := callService(ctx, data.Value); err != nil {
			err := fmt.Errorf("calling service: %v", err)
			span.SetTag("error", true)
			span.LogFields(jaegerlog.Error(err))

			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "OK")
	}
}

func main() {
	cfg := &jaegercfg.Configuration{
		ServiceName: "gateway",
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
	defer transport.Close()

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

	mux := chi.NewRouter()
	mux.Use(cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
		MaxAge:           300, // Maximum value not ignored by any of major browsers
	}).Handler)
	mux.Post("/", apiHandler())

	fmt.Println("starting at http://gateway.localhost")
	h := nethttp.Middleware(tracer, mux,
		nethttp.OperationNameFunc(func(r *http.Request) string {
			return fmt.Sprintf("%s %s %s", r.Proto, r.Method, r.URL.Path)
		}),
	)
	if err := http.ListenAndServe(":http", h); err != http.ErrServerClosed {
		panic(err)
	}
}
