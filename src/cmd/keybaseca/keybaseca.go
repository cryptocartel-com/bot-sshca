package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/keybase/bot-sshca/src/keybaseca/constants"

	"github.com/google/uuid"

	"github.com/keybase/bot-sshca/src/keybaseca/bot"
	"github.com/keybase/bot-sshca/src/keybaseca/config"
	klog "github.com/keybase/bot-sshca/src/keybaseca/log"
	"github.com/keybase/bot-sshca/src/keybaseca/sshutils"
	"github.com/keybase/bot-sshca/src/kssh"
	"github.com/keybase/bot-sshca/src/shared"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

var VersionNumber = "master"

func main() {
	app := cli.NewApp()
	app.Name = "keybaseca"
	app.Usage = "An SSH CA built on top of Keybase"
	app.Version = VersionNumber
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:  "debug",
			Usage: "Log debug information",
		},
		cli.BoolFlag{
			Name:   "wipe-all-configs",
			Hidden: true,
			Usage:  "Clean all client configs the CA Keybase user can find from KBFS",
		},
		cli.BoolFlag{
			Name:   "wipe-logs",
			Hidden: true,
			Usage:  "Used in the integration tests to delete all CA logs",
		},
	}
	app.Commands = []cli.Command{
		{
			Name:   "backup",
			Usage:  "Print the current CA private key to stdout for backup purposes",
			Action: backupAction,
			Before: beforeAction,
		},
		{
			Name:   "generate",
			Usage:  "Generate a new CA key",
			Action: generateAction,
			Before: beforeAction,
		},
		{
			Name:   "service",
			Usage:  "Start the CA service in the foreground",
			Action: serviceAction,
			Before: beforeAction,
		},
		{
			Name:  "sign",
			Usage: "Sign a given public key with all permissions without a dependency on Keybase",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:     "public-key",
					Usage:    "The path to the public key you wish to sign. Eg `~/.ssh/id_rsa.pub`",
					Required: true,
				},
				cli.BoolFlag{
					Name:  "overwrite",
					Usage: "Overwrite the existing certificate on the filesystem",
				},
			},
			Action: signAction,
			Before: beforeAction,
		},
	}
	app.Action = mainAction
	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}

// The action for the `keybaseca backup` subcommand
func backupAction(c *cli.Context) error {
	fmt.Println("Are you sure you want to export the CA private key? If this key is compromised, an " +
		"attacker could access every server that you have configured with this bot. Type \"yes\" to confirm.")
	var response string
	_, err := fmt.Scanln(&response)
	if err != nil {
		return err
	}
	if response != "yes" {
		return fmt.Errorf("Did not get confirmation of key export, aborting")
	}

	conf, err := loadServerConfig()
	if err != nil {
		return err
	}
	bytes, err := ioutil.ReadFile(conf.GetCAKeyLocation())
	if err != nil {
		return fmt.Errorf("Failed to load the CA key from %s: %v", conf.GetCAKeyLocation(), err)
	}
	klog.Log(conf, "Exported CA key to stdout")
	fmt.Println("\nKeep this key somewhere very safe. We recommend keeping a physical copy of it in a secure place.")
	fmt.Println("")
	fmt.Println(string(bytes))
	return nil
}

// The action for the `keybaseca generate` subcommand
func generateAction(c *cli.Context) error {
	conf, err := loadServerConfig()
	if err != nil {
		return err
	}
	captureControlCToDeleteClientConfig(conf)
	err = sshutils.Generate(conf, strings.ToLower(os.Getenv("FORCE_WRITE")) == "true")
	if err != nil {
		return fmt.Errorf("Failed to generate a new key: %v", err)
	}
	return nil
}

// The action for the `keybaseca service` subcommand
func serviceAction(c *cli.Context) error {
	conf, err := loadServerConfigAndWriteClientConfig()
	if err != nil {
		return err
	}
	captureControlCToDeleteClientConfig(conf)
	err = bot.StartBot(conf)
	if err != nil {
		return fmt.Errorf("CA chatbot crashed: %v", err)
	}
	return deleteClientConfig(conf)
}

// The action for the `keybaseca sign` subcommand
func signAction(c *cli.Context) error {
	// Skip validation of the config since that relies on Keybase's servers
	conf := config.EnvConfig{}
	err := config.ValidateConfig(conf, true)
	if err != nil {
		return fmt.Errorf("Invalid config: %v", err)
	}
	principals := strings.Join(conf.GetTeams(), ",")
	expiration := conf.GetKeyExpiration()
	randomUUID, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("Failed to generate unique key ID: %v", err)
	}

	// Read the public key from the specified file
	filename := c.String("public-key")
	pubKey, err := ioutil.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("Failed to read file at %s to get the public key: %v", filename, err)
	}

	// Sign the public key
	signature, err := sshutils.SignKey(conf.GetCAKeyLocation(), randomUUID.String()+":keybaseca-sign", principals, expiration, string(pubKey))
	if err != nil {
		return fmt.Errorf("Failed to sign key: %v", err)
	}

	// Either store it in a file or print it to stdout
	certPath := shared.KeyPathToCert(shared.PubKeyPathToKeyPath(filename))
	_, err = os.Stat(certPath)
	if os.IsNotExist(err) || c.Bool("overwrite") {
		err = ioutil.WriteFile(certPath, []byte(signature), 0600)
		if err != nil {
			return fmt.Errorf("Failed to write certificate to file: %v", err)
		}
		fmt.Printf("Provisioned new certificate in %s\n", certPath)
	} else {
		fmt.Printf("Provisioned new certificate. Place this in %s in order to use it with ssh.\n", certPath)
		fmt.Printf("\n```\n%s```\n", signature)
	}
	return nil
}

// A global before action that handles the --debug flag by setting the logrus logging level
func beforeAction(c *cli.Context) error {
	if c.GlobalBool("debug") {
		logrus.SetLevel(logrus.DebugLevel)
	}
	return nil
}

// The action for the `keybaseca` command. Only used for hidden and unlisted flags.
func mainAction(c *cli.Context) error {
	switch {
	case c.Bool("wipe-all-configs"):
		teams, err := constants.GetDefaultKBFSOperationsStruct().KBFSList("/keybase/team/")
		if err != nil {
			return err
		}

		semaphore := sync.WaitGroup{}
		semaphore.Add(len(teams))
		boundChan := make(chan interface{}, shared.BoundedParallelismLimit)
		teamsFound := []string{}
		teamsFoundMutex := sync.Mutex{}
		for _, team := range teams {
			go func(team string) {
				// Blocks until there is room in boundChan
				boundChan <- 0

				filename := fmt.Sprintf("/keybase/team/%s/%s", team, shared.ConfigFilename)
				exists, _ := constants.GetDefaultKBFSOperationsStruct().KBFSFileExists(filename)
				if exists {
					err = constants.GetDefaultKBFSOperationsStruct().KBFSDelete(filename)
					if err != nil {
						fmt.Printf("%v\n", err)
					}
					teamsFoundMutex.Lock()
					teamsFound = append(teamsFound, team)
					teamsFoundMutex.Unlock()
				}
				semaphore.Done()

				// Make room in boundChan
				<-boundChan
			}(team)
		}
		semaphore.Wait()
		fmt.Printf("Deleted configs found in these teams: %+v\n", teamsFound)
	case c.Bool("wipe-logs"):
		conf, err := loadServerConfig()
		if err != nil {
			return err
		}
		logLocation := conf.GetLogLocation()
		if strings.HasPrefix(logLocation, "/keybase/") {
			err = constants.GetDefaultKBFSOperationsStruct().KBFSDelete(logLocation)
			if err != nil {
				return fmt.Errorf("Failed to delete log file at %s: %v", logLocation, err)
			}
		} else {
			err = os.Remove(logLocation)
			if err != nil {
				return fmt.Errorf("Failed to delete log file at %s: %v", logLocation, err)
			}
		}
		fmt.Println("Wiped existing log file at " + logLocation)
	default:
		cli.ShowAppHelpAndExit(c, 1)
	}
	return nil
}

// Write a kssh config file such that kssh will find it and use it
func writeClientConfig(conf config.Config) error {
	username, err := bot.GetUsername(conf)
	if err != nil {
		return err
	}

	teams := conf.GetTeams()
	if conf.GetChatTeam() != "" {
		// Make sure we place a client config file in the chat team which may not be in the list of teams
		teams = append(teams, conf.GetChatTeam())
	}
	for _, team := range teams {
		filename := filepath.Join("/keybase/team/", team, shared.ConfigFilename)

		var content []byte
		if conf.GetChatTeam() == "" {
			// If they didn't configure a chat team, messages should be sent to any channel. This is done by having each
			// client config reference the team it is found in
			content, err = json.Marshal(kssh.ConfigFile{TeamName: team, BotName: username, ChannelName: ""})
		} else {
			// If they configured a chat team, have messages go there
			content, err = json.Marshal(kssh.ConfigFile{TeamName: conf.GetChatTeam(), BotName: username, ChannelName: conf.GetChannelName()})
		}
		if err != nil {
			return err
		}

		err = constants.GetDefaultKBFSOperationsStruct().KBFSWrite(filename, string(content), false)
		if err != nil {
			return err
		}
	}

	logrus.Debugf("Wrote kssh client config files for the teams: %v", teams)

	return nil
}

// Delete the client config file. Run when the CA bot is terminating so that KBFS does not contain any stale
// client config files
func deleteClientConfig(conf config.Config) error {
	teams := conf.GetTeams()
	if conf.GetChatTeam() != "" {
		// Make sure we delete the client config file in the chat team which may not be in the list of teams
		teams = append(teams, conf.GetChatTeam())
	}

	for _, team := range teams {
		filename := filepath.Join("/keybase/team/", team, shared.ConfigFilename)
		err := constants.GetDefaultKBFSOperationsStruct().KBFSDelete(filename)
		if err != nil {
			return err
		}
	}

	logrus.Debugf("Deleted kssh client config files for the teams: %v", teams)

	return nil
}

// Set up a signal handler in order to catch SIGTERMS that will delete all client config files
// when it receives a sigterm. This ensures that a simple Control-C does not create stale
// client config files
func captureControlCToDeleteClientConfig(conf config.Config) {
	signalChan := make(chan os.Signal)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signalChan
		fmt.Println("losing CA bot...")
		err := deleteClientConfig(conf)
		if err != nil {
			fmt.Printf("Failed to delete client config: %v", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()
}

// Load and validate a server config object from the environment
func loadServerConfig() (config.Config, error) {
	conf := config.EnvConfig{}
	err := config.ValidateConfig(conf, false)
	if err != nil {
		return nil, fmt.Errorf("Failed to validate config: %v", err)
	}
	return &conf, nil
}

func loadServerConfigAndWriteClientConfig() (config.Config, error) {
	conf, err := loadServerConfig()
	if err != nil {
		return nil, err
	}
	err = writeClientConfig(conf)
	if err != nil {
		return nil, fmt.Errorf("Failed to write the client config: %v", err)
	}
	return conf, nil
}
