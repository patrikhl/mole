package cmd

import (
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/davrodpin/mole/alias"
	"github.com/davrodpin/mole/app"
	"github.com/davrodpin/mole/tunnel"

	"github.com/awnumar/memguard"
	daemon "github.com/sevlyar/go-daemon"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	flag "github.com/spf13/pflag"
	"golang.org/x/crypto/ssh/terminal"
)

var (
	aliasName   string
	id          string
	tunnelFlags = &alias.TunnelFlags{}

	rootCmd = &cobra.Command{
		Use:  "mole",
		Long: "Tool to create ssh tunnels focused on resiliency and user experience.",
	}
)

// Execute executes the root command
func Execute() error {
	log.SetOutput(os.Stdout)

	return rootCmd.Execute()
}

func bindFlags(flags *alias.TunnelFlags, cmd *cobra.Command) error {
	cmd.Flags().BoolVarP(&flags.Verbose, "verbose", "v", false, "increase log verbosity")
	cmd.Flags().BoolVarP(&flags.Insecure, "insecure", "i", false, "skip host key validation when connecting to ssh server")
	cmd.Flags().BoolVarP(&flags.Detach, "detach", "x", false, "run process in background")
	cmd.Flags().VarP(&flags.Source, "source", "S", `set source endpoint address: [<host>]:<port>
multiple -source flags can be provided`)
	cmd.Flags().VarP(&flags.Destination, "destination", "d", `set destination endpoint address: [<host>]:<port>
multiple -destination flags can be provided`)
	cmd.Flags().VarP(&flags.Server, "server", "s", "set server address: [<user>@]<host>[:<port>]")
	cmd.Flags().StringVarP(&flags.Key, "key", "k", "", "set server authentication key file path")
	cmd.Flags().DurationVarP(&flags.KeepAliveInterval, "keep-alive-interval", "K", 10*time.Second, "time interval for keep alive packets to be sent")
	cmd.Flags().IntVarP(&flags.ConnectionRetries, "connection-retries", "R", 3, `maximum number of connection retries to the ssh server
provide 0 to never give up or a negative number to disable`)
	cmd.Flags().DurationVarP(&flags.WaitAndRetry, "retry-wait", "w", 3*time.Second, "time to wait before trying to reconnect to ssh server")
	cmd.Flags().StringVarP(&flags.SshAgent, "ssh-agent", "A", "", "unix socket to communicate with a ssh agent")
	cmd.Flags().DurationVarP(&flags.Timeout, "timeout", "t", 3*time.Second, "ssh server connection timeout")
	cmd.Flags().StringVarP(&flags.Config, "config", "c", "$HOME/.ssh/config", "set config file path")

	err := cmd.MarkFlagRequired("server")
	if err != nil {
		return err
	}

	flag.Visit(func(f *flag.Flag) {
		flags.GivenFlags = append(flags.GivenFlags, f.Name)
	})

	return nil
}

func start(id string, tunnelFlags *alias.TunnelFlags) {
	// memguard is used to securely keep sensitive information in memory.
	// This call makes sure all data will be destroy when the program exits.
	defer memguard.Purge()

	if tunnelFlags.Detach {
		var err error

		ic, err := app.NewDetachedInstance(id)
		if err != nil {
			log.WithError(err).Errorf("error while creating directory to store mole instance related files")
			os.Exit(1)
		}

		err = startDaemonProcess(ic)
		if err != nil {
			log.WithFields(log.Fields{
				"id": id,
			}).WithError(err).Error("error starting ssh tunnel")
			os.Exit(1)
		}
	}

	if tunnelFlags.Verbose {
		log.SetLevel(log.DebugLevel)
	}

	s, err := tunnel.NewServer(tunnelFlags.Server.User, tunnelFlags.Server.Address(), tunnelFlags.Key, tunnelFlags.SshAgent, tunnelFlags.Config)
	if err != nil {
		log.Errorf("error processing server options: %v\n", err)
		os.Exit(1)
	}

	s.Insecure = tunnelFlags.Insecure
	s.Timeout = tunnelFlags.Timeout

	err = s.Key.HandlePassphrase(func() ([]byte, error) {
		fmt.Printf("The key provided is secured by a password. Please provide it below:\n")
		fmt.Printf("Password: ")
		p, err := terminal.ReadPassword(int(syscall.Stdin))
		fmt.Printf("\n")
		return p, err
	})

	if err != nil {
		log.WithError(err).Error("error setting up password handling function")
		os.Exit(1)
	}

	log.Debugf("server: %s", s)

	source := make([]string, len(tunnelFlags.Source))
	for i, r := range tunnelFlags.Source {
		source[i] = r.String()
	}

	destination := make([]string, len(tunnelFlags.Destination))
	for i, r := range tunnelFlags.Destination {
		if r.Port == "" {
			err := fmt.Errorf("missing port in destination address: %s", r.String())
			log.Error(err)
			os.Exit(1)
		}

		destination[i] = r.String()
	}

	t, err := tunnel.New(tunnelFlags.TunnelType, s, source, destination, tunnelFlags.Config)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	//TODO need to find a way to require the attributes below to be always set
	// since they are not optional (functionality will break if they are not
	// set and CLI parsing is the one setting the default values).
	// That could be done by make them required in the constructor's signature
	t.ConnectionRetries = tunnelFlags.ConnectionRetries
	t.WaitAndRetry = tunnelFlags.WaitAndRetry
	t.KeepAliveInterval = tunnelFlags.KeepAliveInterval

	if err = t.Start(); err != nil {
		log.WithFields(log.Fields{
			"tunnel": t.String(),
		}).Errorf("%v", err)

		os.Exit(1)
	}
}

func startDaemonProcess(instanceConf *app.DetachedInstance) error {
	cntxt := &daemon.Context{
		PidFileName: app.InstancePidFile,
		PidFilePerm: 0644,
		LogFileName: app.InstanceLogFile,
		LogFilePerm: 0640,
		Umask:       027,
		Args:        os.Args,
	}

	d, err := cntxt.Reborn()
	if err != nil {
		return err
	}

	if d != nil {
		err = os.Rename(app.InstancePidFile, instanceConf.PidFile)
		if err != nil {
			return err
		}

		err = os.Rename(app.InstanceLogFile, instanceConf.LogFile)
		if err != nil {
			return err
		}

		log.Infof("execute \"mole stop %s\" if you like to stop it at any time", instanceConf.Id)

		os.Exit(0)
	}

	defer cntxt.Release()

	return nil
}

func startFromAlias(aliasName string, a *alias.Alias) error {
	f, err := a.ParseTunnelFlags()
	if err != nil {
		return err
	}

	start(aliasName, f)

	return nil
}

func stop(id string) error {
	pfp, err := app.GetPidFileLocation(id)
	if err != nil {
		return fmt.Errorf("error getting information about aliases directory: %v", err)
	}

	if _, err := os.Stat(pfp); os.IsNotExist(err) {
		return fmt.Errorf("no instance of mole with id %s is running", id)
	}

	cntxt := &daemon.Context{
		PidFileName: pfp,
	}

	d, err := cntxt.Search()
	if err != nil {
		return err
	}

	err = d.Kill()
	if err != nil {
		return err
	}

	err = os.RemoveAll(pfp)
	if err != nil {
		return err
	}

	return nil
}
