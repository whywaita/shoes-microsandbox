package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	microsandbox "github.com/superradcompany/microsandbox/sdk/go"
	"github.com/hashicorp/go-plugin"
	pb "github.com/whywaita/myshoes/api/proto.go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Environment variable keys for configuration
const (
	EnvMSBImage            = "MSB_IMAGE"            // OCI image for runners
	EnvMSBMemoryMiB        = "MSB_MEMORY_MIB"        // Memory per sandbox in MiB
	EnvMSBCPUs             = "MSB_CPUS"              // Number of CPUs per sandbox
	EnvMSBRunnerUser       = "MSB_RUNNER_USER"      // User inside VM
	EnvMSBRegistryUsername = "MSB_REGISTRY_USERNAME" // Private registry username
	EnvMSBRegistryPassword = "MSB_REGISTRY_PASSWORD" // Private registry password
	EnvMSBPullPolicy       = "MSB_PULL_POLICY"       // Image pull policy: "always", "if-missing", "never"
)

// Default configuration values
const (
	DefaultImage      = "ghcr.io/actions/actions-runner:latest"
	DefaultMemoryMiB  = 2048
	DefaultCPUs       = 2
	DefaultRunnerUser = "runner"
	ShoesType         = "shoes-microsandbox"
)

func main() {
	handshake := plugin.HandshakeConfig{
		ProtocolVersion:  1,
		MagicCookieKey:   "SHOES_PLUGIN_MAGIC_COOKIE",
		MagicCookieValue: "are_you_a_shoes?",
	}
	pluginMap := map[string]plugin.Plugin{
		"shoes_grpc": &MicrosandboxPlugin{},
	}

	plugin.Serve(&plugin.ServeConfig{
		HandshakeConfig: handshake,
		Plugins:         pluginMap,
		GRPCServer:      plugin.DefaultGRPCServer,
	})
}

// MicrosandboxPlugin implements the hashicorp go-plugin Plugin interface
type MicrosandboxPlugin struct {
	plugin.Plugin
}

// GRPCServer creates and registers the gRPC server
func (p *MicrosandboxPlugin) GRPCServer(broker *plugin.GRPCBroker, s *grpc.Server) error {
	server, err := NewMicrosandboxServer()
	if err != nil {
		return fmt.Errorf("failed to create microsandbox server: %w", err)
	}
	pb.RegisterShoesServer(s, server)
	return nil
}

// GRPCClient is not used (this is a server-only plugin)
func (p *MicrosandboxPlugin) GRPCClient(ctx context.Context, broker *plugin.GRPCBroker, c *grpc.ClientConn) (interface{}, error) {
	return nil, nil
}

// MicrosandboxServer implements the Shoes gRPC service using microsandbox
type MicrosandboxServer struct {
	pb.UnimplementedShoesServer
	image             string
	memoryMiB         uint32
	cpus              uint8
	runnerUser        string
	registryAuth      *microsandbox.RegistryAuth
	pullPolicy        microsandbox.PullPolicy
	// sandboxIndex maps cloudID -> sandbox name for tracking
	// We use the cloudID as the sandbox name directly for simplicity
}

// NewMicrosandboxServer creates a new MicrosandboxServer from environment config
func NewMicrosandboxServer() (*MicrosandboxServer, error) {
	image := envOrDefault(EnvMSBImage, DefaultImage)
	memoryMiB, err := envOrDefaultInt(EnvMSBMemoryMiB, DefaultMemoryMiB)
	if err != nil {
		slog.Warn("using default for memory", "error", err)
	}
	cpus, err := envOrDefaultInt(EnvMSBCPUs, DefaultCPUs)
	if err != nil {
		slog.Warn("using default for cpus", "error", err)
	}
	runnerUser := envOrDefault(EnvMSBRunnerUser, DefaultRunnerUser)

	// Optional: private registry credentials
	var registryAuth *microsandbox.RegistryAuth
	if username := os.Getenv(EnvMSBRegistryUsername); username != "" {
		registryAuth = &microsandbox.RegistryAuth{
			Username: username,
			Password: os.Getenv(EnvMSBRegistryPassword),
		}
	}

	// Optional: pull policy (default: "" = if-missing)
	var pullPolicy microsandbox.PullPolicy
	if pp := os.Getenv(EnvMSBPullPolicy); pp != "" {
		pullPolicy = microsandbox.PullPolicy(pp)
	}

	// Ensure the microsandbox runtime is installed
	ctx := context.Background()
	if err := microsandbox.EnsureInstalled(ctx); err != nil {
		return nil, fmt.Errorf("failed to install microsandbox runtime: %w", err)
	}
	slog.Info("microsandbox runtime is ready", "version", microsandbox.SDKVersion())

	return &MicrosandboxServer{
		image:        image,
		memoryMiB:    uint32(memoryMiB),
		cpus:         uint8(cpus),
		runnerUser:   runnerUser,
		registryAuth: registryAuth,
		pullPolicy:   pullPolicy,
	}, nil
}

// AddInstance creates a new microsandbox VM, runs the setup script, and returns connection info
func (s *MicrosandboxServer) AddInstance(ctx context.Context, req *pb.AddInstanceRequest) (*pb.AddInstanceResponse, error) {
	logger := slog.Default()
	logger.Info("AddInstance called",
		"runner_name", req.RunnerName,
		"resource_type", req.ResourceType,
		"labels", req.Labels,
	)

	// Use runnerName as the sandbox name (cloud ID)
	sandboxName := fmt.Sprintf("myshoes-%s", req.RunnerName)

	// Calculate resources based on resource type
	memoryMiB, cpus := s.resourceConfig(req.ResourceType)

	opts := []microsandbox.SandboxOption{
		microsandbox.WithImage(s.image),
		microsandbox.WithMemory(memoryMiB),
		microsandbox.WithCPUs(cpus),
		microsandbox.WithHostname(sandboxName),
		microsandbox.WithDetached(), // VM outlives this process — required for long-running runners
		microsandbox.WithReplace(),  // Replace if a sandbox with this name already exists
	}

	// Apply registry credentials if configured
	if s.registryAuth != nil {
		opts = append(opts, microsandbox.WithRegistryAuth(*s.registryAuth))
	}

	// Apply pull policy if configured (default: "" = if-missing)
	if s.pullPolicy != "" {
		opts = append(opts, microsandbox.WithPullPolicy(s.pullPolicy))
	}

	// Create and boot the sandbox
	sb, err := microsandbox.CreateSandbox(ctx, sandboxName, opts...)
	if err != nil {
		logger.Error("failed to create sandbox",
			"runner_name", req.RunnerName,
			"error", err,
		)
		return nil, status.Errorf(codes.Internal, "failed to create sandbox: %v", err)
	}

	// Run the setup script inside the sandbox
	if req.SetupScript != "" {
		output, err := sb.Exec(ctx, "/bin/sh", []string{"-c", req.SetupScript},
			microsandbox.WithExecUser(s.runnerUser),
		)
		if err != nil {
			logger.Error("failed to run setup script",
				"runner_name", req.RunnerName,
				"error", err,
			)
			// Clean up the sandbox on failure
			if _, stopErr := sb.StopAndWait(ctx); stopErr != nil {
				logger.Warn("failed to stop sandbox after setup script error",
					"error", stopErr)
			}
			return nil, status.Errorf(codes.Internal, "failed to run setup script: %v", err)
		}
		if !output.Success() {
			logger.Error("setup script exited non-zero",
				"runner_name", req.RunnerName,
				"exit_code", output.ExitCode(),
			)
			// Clean up the sandbox on setup script failure
			if _, stopErr := sb.StopAndWait(ctx); stopErr != nil {
				logger.Warn("failed to stop sandbox after setup script failure",
					"error", stopErr)
			}
			return nil, status.Errorf(codes.Internal, "setup script exited with code %d", output.ExitCode())
		}
	}

	// Get the IP address of the sandbox
	// microsandbox assigns IPs internally; we expose it via the sandbox handle
	ipAddress, err := s.getSandboxIP(ctx, sb)
	if err != nil {
		logger.Warn("could not determine sandbox IP, using empty string",
			"error", err,
		)
		ipAddress = ""
	}

	// Detach from sandbox — it keeps running independently
	// We'll reconnect by name for DeleteInstance
	if err := sb.Detach(ctx); err != nil {
		logger.Warn("failed to detach sandbox, stopping instead",
			"error", err,
		)
		// If detach fails, the sandbox will be cleaned up when this process exits
		// which is not ideal, so we log a warning
	}

	cloudID := sandboxName

	logger.Info("sandbox created successfully",
		"runner_name", req.RunnerName,
		"cloud_id", cloudID,
		"ip_address", ipAddress,
	)

	return &pb.AddInstanceResponse{
		CloudId:      cloudID,
		ShoesType:    ShoesType,
		IpAddress:    ipAddress,
		ResourceType: resourceTypeToPb(req.ResourceType),
	}, nil
}

// DeleteInstance stops and removes a sandbox
func (s *MicrosandboxServer) DeleteInstance(ctx context.Context, req *pb.DeleteInstanceRequest) (*pb.DeleteInstanceResponse, error) {
	logger := slog.Default()
	logger.Info("DeleteInstance called",
		"cloud_id", req.CloudId,
	)

	// Validate that cloudID has the expected prefix to prevent targeting arbitrary sandboxes
	if !strings.HasPrefix(req.CloudId, "myshoes-") {
		return nil, status.Errorf(codes.InvalidArgument, "invalid cloud_id: must start with 'myshoes-', got %q", req.CloudId)
	}

	// Look up the sandbox by name (cloudID == sandbox name)
	handle, err := microsandbox.GetSandbox(ctx, req.CloudId)
	if err != nil {
		if microsandbox.IsKind(err, microsandbox.ErrSandboxNotFound) {
			// Sandbox already gone — treat as success
			logger.Info("sandbox already removed",
				"cloud_id", req.CloudId,
			)
			return &pb.DeleteInstanceResponse{}, nil
		}
		logger.Error("failed to find sandbox",
			"cloud_id", req.CloudId,
			"error", err,
		)
		return nil, status.Errorf(codes.Internal, "failed to find sandbox: %v", err)
	}

	// Stop the sandbox
	if err := handle.Stop(ctx); err != nil {
		logger.Warn("failed to stop sandbox, trying kill",
			"cloud_id", req.CloudId,
			"error", err,
		)
		// Try to kill if stop fails
		if killErr := handle.Kill(ctx); killErr != nil {
			logger.Error("failed to kill sandbox",
				"cloud_id", req.CloudId,
				"error", killErr,
			)
			return nil, status.Errorf(codes.Internal, "failed to stop/kill sandbox: %v", err)
		}
	}

	// Remove persisted state
	if err := handle.Remove(ctx); err != nil {
		logger.Warn("failed to remove sandbox state",
			"cloud_id", req.CloudId,
			"error", err,
		)
		// Not critical — state cleanup is best-effort
	}

	logger.Info("sandbox deleted successfully",
		"cloud_id", req.CloudId,
	)

	return &pb.DeleteInstanceResponse{}, nil
}

// resourceConfig returns (memoryMiB, cpus) based on the resource type
func (s *MicrosandboxServer) resourceConfig(rt pb.ResourceType) (uint32, uint8) {
	switch rt {
	case pb.ResourceType_Nano:
		return 512, 1
	case pb.ResourceType_Micro:
		return 1024, 1
	case pb.ResourceType_Small:
		return 2048, 2
	case pb.ResourceType_Medium:
		return 4096, 2
	case pb.ResourceType_Large:
		return 8192, 4
	case pb.ResourceType_XLarge:
		return 16384, 4
	case pb.ResourceType_XLarge2:
		return 32768, 8
	case pb.ResourceType_XLarge3:
		return 65536, 8
	case pb.ResourceType_XLarge4:
		return 131072, 16
	default:
		return s.memoryMiB, s.cpus
	}
}

// getSandboxIP attempts to determine the sandbox's IP address
func (s *MicrosandboxServer) getSandboxIP(ctx context.Context, sb *microsandbox.Sandbox) (string, error) {
	// Try to get the IP from the sandbox by running `hostname -I`
	output, err := sb.Exec(ctx, "/bin/sh", []string{"-c", "hostname -I 2>/dev/null | awk '{print $1}'"})
	if err != nil {
		return "", fmt.Errorf("failed to get IP: %w", err)
	}
	if ip := strings.TrimSpace(output.Stdout()); ip != "" {
		return ip, nil
	}

	// Fallback: try `ip addr` (POSIX-compatible — no grep -P)
	output, err = sb.Exec(ctx, "/bin/sh", []string{"-c",
		"ip -4 addr show eth0 2>/dev/null | awk '/inet /{print $2}' | cut -d/ -f1"})
	if err != nil {
		return "", fmt.Errorf("failed to get IP via ip addr: %w", err)
	}
	if ip := strings.TrimSpace(output.Stdout()); ip != "" {
		return ip, nil
	}
	return "", fmt.Errorf("could not determine sandbox IP address")
}

// resourceTypeToPb converts the proto resource type, defaulting to the configured type
func resourceTypeToPb(rt pb.ResourceType) pb.ResourceType {
	if rt == pb.ResourceType_Unknown {
		return pb.ResourceType_Small
	}
	return rt
}

// envOrDefault reads an environment variable or returns the default value
func envOrDefault(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

// envOrDefaultInt reads an environment variable as a positive int or returns the default value.
// Returns an error if the value is non-numeric, negative, or zero.
func envOrDefaultInt(key string, defaultVal int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal, nil
	}
	intVal, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal, fmt.Errorf("invalid integer value for %s: %q, using default %d: %w", key, v, defaultVal, err)
	}
	if intVal <= 0 {
		return defaultVal, fmt.Errorf("value for %s must be positive, got %d, using default %d", key, intVal, defaultVal)
	}
	return intVal, nil
}

// Ensure MicrosandboxServer implements the ShoesServer interface at compile time
var _ pb.ShoesServer = (*MicrosandboxServer)(nil)