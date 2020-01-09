package main

import (
	"context"
	"database/sql"
	"log"
	"net"
	"net/http"
	"storage/pb"
	"strings"

	"github.com/go-chi/cors"
	"github.com/golang/protobuf/ptypes/empty"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	grpc_opentracing "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	"github.com/grpc-ecosystem/grpc-gateway/runtime"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
	"github.com/opentracing/opentracing-go"
	jaegerlog "github.com/opentracing/opentracing-go/log"
	"github.com/soheilhy/cmux"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
)

type ResourceService struct {
	db *sqlx.DB
}

func (r *ResourceService) Save(ctx context.Context, req *pb.SaveResourceRequest) (*pb.Resource, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "resource-save")
	defer span.Finish()

	var res pb.Resource
	row := r.db.QueryRowxContext(ctx, `INSERT INTO resource (id, value) VALUES (DEFAULT, $1) RETURNING id, value`, req.Value)
	if err := row.StructScan(&res); err != nil {
		span.SetTag("error", true)
		span.LogFields(jaegerlog.Error(err))
		return nil, err
	}

	return &res, nil
}

func (r *ResourceService) List(_ *pb.ListResourceRequest, resp pb.ResourceService_ListServer) error {
	span, ctx := opentracing.StartSpanFromContext(resp.Context(), "resource-list")
	defer span.Finish()

	rows, err := r.db.QueryxContext(ctx, `SELECT * FROM resource`)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var r pb.Resource
		if err = rows.StructScan(&r); err != nil {
			return err
		}
		if err := resp.Send(&r); err != nil {
			return err
		}
	}

	return nil
}

func (r *ResourceService) GetAll(ctx context.Context, _ *pb.GetResourceRequest) (*pb.ResourceList, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "resource-getAll")
	defer span.Finish()

	list := make([]*pb.Resource, 0)
	if err := sqlx.SelectContext(ctx, r.db, &list, `SELECT * FROM resource`); err != nil {
		return nil, err
	}
	return &pb.ResourceList{List: list}, nil
}

func (r *ResourceService) Get(ctx context.Context, req *pb.GetResourceRequest) (*pb.Resource, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "resource-get")
	defer span.Finish()

	var res pb.Resource
	if err := sqlx.GetContext(ctx, r.db, &res, `SELECT id, value FROM resource WHERE id = $1`, req.ResourceId); err != nil {
		if err == sql.ErrNoRows {
			return nil, status.Errorf(codes.NotFound, err.Error())
		}
		return nil, err
	}
	return &res, nil
}

func (r *ResourceService) Delete(ctx context.Context, req *pb.DeleteResourceRequest) (*empty.Empty, error) {
	span, ctx := opentracing.StartSpanFromContext(ctx, "resource-delete")
	defer span.Finish()

	if _, err := r.db.ExecContext(ctx, `DELETE FROM resource WHERE id = $1`, req.ResourceId); err != nil {
		return nil, err
	}
	return &empty.Empty{}, nil
}

func prepareGRPC(svc *ResourceService, tracer opentracing.Tracer) (*grpc.Server, error) {
	g := grpc.NewServer(
		grpc_middleware.WithStreamServerChain(grpc_opentracing.StreamServerInterceptor(grpc_opentracing.WithTracer(tracer))),
		grpc_middleware.WithUnaryServerChain(grpc_opentracing.UnaryServerInterceptor(grpc_opentracing.WithTracer(tracer))),
	)
	pb.RegisterResourceServiceServer(g, svc)
	reflection.Register(g)

	return g, nil
}

func prepareREST(svc *ResourceService) (*http.Server, error) {
	mux := runtime.NewServeMux(runtime.WithMarshalerOption(runtime.MIMEWildcard, &runtime.JSONPb{OrigName: true, EmitDefaults: true}))
	if err := pb.RegisterResourceServiceHandlerServer(context.TODO(), mux, svc); err != nil {
		return nil, err
	}

	return &http.Server{Handler: cors.New(cors.Options{
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"*"},
		AllowCredentials: true,
		MaxAge:           300, // Maximum value not ignored by any of major browsers
	}).Handler(mux)}, nil
}

func main() {
	cfg := &jaegercfg.Configuration{
		ServiceName: "storage",
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

	db, err := sqlx.Connect("postgres", "postgres://root@cockroach:26257?sslmode=disable")
	if err != nil {
		log.Printf("Could not connect to cockroach: %s", err.Error())
		return
	}
	defer db.Close()

	if _, err := db.Exec(`DROP TABLE IF EXISTS resource`); err != nil {
		log.Printf("Could not connect to cockroach: %s", err.Error())
		return
	}
	if _, err := db.Exec(`CREATE TABLE resource (
    	id		UUID PRIMARY KEY NOT NULL DEFAULT gen_random_uuid(),
    	value   STRING NOT NULL
	)`); err != nil {
		log.Printf("Could not connect to cockroach: %s", err.Error())
		return
	}

	l, err := net.Listen("tcp", ":http")
	if err != nil {
		panic(err)
	}
	defer l.Close()

	tcpMux := cmux.New(l)
	grpcL := tcpMux.MatchWithWriters(cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
	restL := tcpMux.Match(cmux.HTTP1Fast())

	svc := ResourceService{db: db}

	grpcs, err := prepareGRPC(&svc, tracer)
	if err != nil {
		log.Fatal(err)
	}

	rests, err := prepareREST(&svc)
	if err != nil {
		log.Fatal(err)
	}

	go func() {
		if err = grpcs.Serve(grpcL); err != cmux.ErrListenerClosed {
			log.Fatal(err)
		}
	}()
	go func() {
		if err = rests.Serve(restL); err != cmux.ErrListenerClosed {
			log.Fatal(err)
		}
	}()

	if err := tcpMux.Serve(); !strings.Contains(err.Error(), "use of closed network connection") {
		log.Fatal(err)
	}
}
