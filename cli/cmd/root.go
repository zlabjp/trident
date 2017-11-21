package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/netapp/trident/config"
	"github.com/spf13/cobra"
	k8s "k8s.io/api/core/v1"
)

const (
	FORMAT_JSON = "json"
	FORMAT_NAME = "name"
	FORMAT_WIDE = "wide"
	FORMAT_YAML = "yaml"

	MODE_DIRECT = "direct"
	MODE_TUNNEL = "tunnel"
	MODE_LOGS   = "logs"

	CLI_KUBERNETES = "kubectl"
	CLI_OPENSHIFT  = "oc"

	POD_SERVER = "127.0.0.1:8000"

	EXIT_CODE_SUCCESS = 0
	EXIT_CODE_FAILURE = 1
)

var (
	OperatingMode       string
	KubernetesCLI       string
	TridentPodName      string
	TridentPodNamespace string
	ExitCode            int

	Debug        bool
	Server       string
	OutputFormat string
)

var RootCmd = &cobra.Command{
	SilenceUsage: true,
	Use:          "tridentctl",
	Short:        "A CLI tool for NetApp Trident",
	Long:         `A CLI tool for managing the NetApp Trident external storage provisioner for Kubernetes`,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		err := discoverOperatingMode(cmd)
		return err
	},
}

func init() {
	RootCmd.PersistentFlags().BoolVarP(&Debug, "debug", "d", false, "Debug output")
	RootCmd.PersistentFlags().StringVarP(&Server, "server", "s", "", "Address/port of Trident REST interface")
	RootCmd.PersistentFlags().StringVarP(&OutputFormat, "output", "o", "", "Output format. One of json|yaml|name|wide|ps (default)")
	RootCmd.PersistentFlags().StringVarP(&TridentPodNamespace, "namespace", "n", "", "Namespace of Trident deployment")
}

func discoverOperatingMode(cmd *cobra.Command) error {

	defer func() {
		if !Debug {
			return
		}

		switch OperatingMode {
		case MODE_DIRECT:
			fmt.Printf("Operating mode = %s, Server = %s\n", OperatingMode, Server)
		case MODE_TUNNEL:
			fmt.Printf("Operating mode = %s, Trident pod = %s, Namespace = %s, CLI = %s\n",
				OperatingMode, TridentPodName, TridentPodNamespace, KubernetesCLI)
		case MODE_LOGS:
			fmt.Printf("Operating mode = %s, Namespace = %s, CLI = %s\n",
				OperatingMode, TridentPodNamespace, KubernetesCLI)
		}
	}()

	var err error

	envServer := os.Getenv("TRIDENT_SERVER")

	if Server != "" {

		// Server specified on command line takes precedence
		OperatingMode = MODE_DIRECT
		return nil
	} else if envServer != "" {

		// Consider environment variable next
		Server = envServer
		OperatingMode = MODE_DIRECT
		return nil
	}

	// To work with pods, we need to discover which CLI to invoke
	err = discoverKubernetesCLI()
	if err != nil {
		return err
	}

	// Server not specified, so try tunneling to a pod
	if TridentPodNamespace == "" {
		TridentPodNamespace, err = getCurrentNamespace()
		if err != nil {
			return err
		}
	}

	TridentPodName, err = getTridentPod(TridentPodNamespace)
	if err != nil {
		// If we're running 'logs', and there isn't a Trident pod, set a special mode
		// so we don't terminate execution before we even start.
		if cmd.Name() == "logs" {
			OperatingMode = MODE_LOGS
			return nil
		}
		return err
	}

	OperatingMode = MODE_TUNNEL
	Server = POD_SERVER
	return nil
}

func discoverKubernetesCLI() error {

	// Try the OpenShift CLI first
	_, err := exec.Command(CLI_OPENSHIFT, "version").CombinedOutput()
	if GetExitCodeFromError(err) == EXIT_CODE_SUCCESS {
		KubernetesCLI = CLI_OPENSHIFT
		return nil
	}

	// Fall back to the K8S CLI
	_, err = exec.Command(CLI_KUBERNETES, "version").CombinedOutput()
	if GetExitCodeFromError(err) == EXIT_CODE_SUCCESS {
		KubernetesCLI = CLI_KUBERNETES
		return nil
	}

	return errors.New("Could not find the Kubernetes CLI.")
}

func getCurrentNamespace() (string, error) {

	// Get current namespace from service account info
	cmd := exec.Command(KubernetesCLI, "get", "serviceaccount", "default", "-o=json")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var serviceAccount k8s.ServiceAccount
	if err := json.NewDecoder(stdout).Decode(&serviceAccount); err != nil {
		return "", err
	}
	if err := cmd.Wait(); err != nil {
		return "", err
	}

	//fmt.Printf("%+v\n", serviceAccount)

	// Get Trident pod name & namespace
	namespace := serviceAccount.ObjectMeta.Namespace

	return namespace, nil
}

func getTridentPod(namespace string) (string, error) {

	// Get 'trident' pod info
	cmd := exec.Command(KubernetesCLI, "get", "pod", "-n", namespace, "-l", "app=trident.netapp.io", "-o=json")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var tridentPod k8s.PodList
	if err := json.NewDecoder(stdout).Decode(&tridentPod); err != nil {
		return "", err
	}
	if err := cmd.Wait(); err != nil {
		return "", err
	}

	//fmt.Printf("%+v\n", tridentPod)

	if len(tridentPod.Items) != 1 {
		return "", fmt.Errorf("could not find a Trident pod in the %s namespace. "+
			"You may need to use the -n option to specify the correct namespace.",
			namespace)
	}

	// Get Trident pod name & namespace
	name := tridentPod.Items[0].ObjectMeta.Name

	return name, nil
}

func GetBaseURL() (string, error) {

	url := fmt.Sprintf("http://%s%s", Server, config.BaseURL)

	if Debug {
		fmt.Printf("Trident URL: %s\n", url)
	}

	return url, nil
}

func TunnelCommand(commandArgs []string) {

	// Build tunnel command to exec command in container
	execCommand := []string{"exec", TridentPodName, "-n", TridentPodNamespace, "-c", config.ContainerTrident, "--"}

	// Build CLI command
	cliCommand := []string{"tridentctl", "-s", Server}
	if Debug {
		cliCommand = append(cliCommand, "--debug")
	}
	if OutputFormat != "" {
		cliCommand = append(cliCommand, []string{"--output", OutputFormat}...)
	}
	cliCommand = append(cliCommand, commandArgs...)

	// Combine tunnel and CLI commands
	execCommand = append(execCommand, cliCommand...)

	if Debug {
		fmt.Printf("Invoking tunneled command: %s %v\n", KubernetesCLI, strings.Join(execCommand, " "))
	}

	// Invoke tridentctl inside the Trident pod
	out, err := exec.Command(KubernetesCLI, execCommand...).CombinedOutput()

	SetExitCodeFromError(err)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s", string(out))
	} else {
		fmt.Print(string(out))
	}
}

func TunnelCommandRaw(commandArgs []string) ([]byte, error) {

	// Build tunnel command to exec command in container
	execCommand := []string{"exec", TridentPodName, "-n", TridentPodNamespace, "-c", config.ContainerTrident, "--"}

	// Build CLI command
	cliCommand := []string{"tridentctl", "-s", Server}
	cliCommand = append(cliCommand, commandArgs...)

	// Combine tunnel and CLI commands
	execCommand = append(execCommand, cliCommand...)

	if Debug {
		fmt.Printf("Invoking tunneled command: %s %v\n", KubernetesCLI, strings.Join(execCommand, " "))
	}

	// Invoke tridentctl inside the Trident pod
	output, err := exec.Command(KubernetesCLI, execCommand...).CombinedOutput()

	SetExitCodeFromError(err)
	return output, err
}

func SetExitCodeFromError(err error) {
	ExitCode = GetExitCodeFromError(err)
}

func GetExitCodeFromError(err error) int {
	if err == nil {
		return EXIT_CODE_SUCCESS
	} else {

		// Default to 1 in case we can't determine a process exit code
		code := EXIT_CODE_FAILURE

		if exitError, ok := err.(*exec.ExitError); ok {
			ws := exitError.Sys().(syscall.WaitStatus)
			code = ws.ExitStatus()
		}

		return code
	}
}
