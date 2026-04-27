package driver

type Config struct {
	Namespace            string
	SupervisorImage      string
	SupervisorBinaryPath string
	SupervisorMountPath  string
	GatewayEndpoint      string
}

func DefaultConfig() Config {
	return Config{
		Namespace:            "openshell-system",
		SupervisorImage:      "quay.io/azaalouk/openshell-supervisor:latest",
		SupervisorBinaryPath: "/usr/local/bin/openshell-sandbox",
		SupervisorMountPath:  "/opt/openshell/bin",
	}
}
