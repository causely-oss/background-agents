package agent

import (
	"context"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const (
	agentName                = "causely-root-cause-investigator"
	agentWorkflowName        = "investigate-causely-root-cause"
	telemetryInstrumentation = "github.com/causely-oss/background-agents/causely-background-agent"
)

// initTelemetry enables OTLP only when an endpoint is configured, keeping
// tests and local runs independent from a collector.
func initTelemetry(ctx context.Context) (func(context.Context) error, error) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracegrpc.New(ctx)
	if err != nil {
		return nil, err
	}
	res, err := resource.New(ctx,
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName("causely-background-agent"),
			semconv.K8SPodName(os.Getenv("K8S_POD_NAME")),
			semconv.K8SPodUID(os.Getenv("K8S_POD_UID")),
			semconv.K8SNamespaceName(os.Getenv("K8S_NAMESPACE_NAME")),
			semconv.K8SDeploymentName(os.Getenv("K8S_DEPLOYMENT_NAME")),
			semconv.K8SContainerName(os.Getenv("K8S_CONTAINER_NAME")),
			semconv.K8SNodeName(os.Getenv("K8S_NODE_NAME")),
		),
	)
	if err != nil {
		return nil, err
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return provider.Shutdown, nil
}
