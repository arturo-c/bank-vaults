// Copyright © 2019 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"bytes"
	"path/filepath"
	"text/template"
	"time"

	"github.com/Masterminds/sprig"
	"github.com/banzaicloud/bank-vaults/pkg/vault"
	"github.com/fsnotify/fsnotify"
	"github.com/hashicorp/vault/api"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

const cfgVaultConfigFile = "vault-config-file"

var configureCmd = &cobra.Command{
	Use:   "configure",
	Short: "Configures a Vault based on a YAML/JSON configuration file",
	Long: `This configuration is an extension to what is available through the Vault configuration:
			https://www.vaultproject.io/docs/configuration/index.html. With this it is possible to
			configure secret engines, auth methods, etc...`,
	Run: func(cmd *cobra.Command, args []string) {
		appConfig.BindPFlag(cfgOnce, cmd.PersistentFlags().Lookup(cfgOnce))
		appConfig.BindPFlag(cfgUnsealPeriod, cmd.PersistentFlags().Lookup(cfgUnsealPeriod))
		appConfig.BindPFlag(cfgVaultConfigFile, cmd.PersistentFlags().Lookup(cfgVaultConfigFile))

		runOnce := appConfig.GetBool(cfgOnce)
		unsealConfig.unsealPeriod = appConfig.GetDuration(cfgUnsealPeriod)
		vaultConfigFiles := appConfig.GetStringSlice(cfgVaultConfigFile)

		store, err := kvStoreForConfig(appConfig)

		if err != nil {
			logrus.Fatalf("error creating kv store: %s", err.Error())
		}

		cl, err := api.NewClient(nil)

		if err != nil {
			logrus.Fatalf("error connecting to vault: %s", err.Error())
		}

		vaultConfig, err := vaultConfigForConfig(appConfig)

		if err != nil {
			logrus.Fatalf("error building vault config: %s", err.Error())
		}

		v, err := vault.New(store, cl, vaultConfig)

		if err != nil {
			logrus.Fatalf("error creating vault helper: %s", err.Error())
		}

		configurations := make(chan *viper.Viper, len(vaultConfigFiles))

		for _, vaultConfigFile := range vaultConfigFiles {
			configurations <- parseConfiguration(vaultConfigFile)
		}

		if !runOnce {
			go watchConfigurations(vaultConfigFiles, configurations)
		} else {
			close(configurations)
		}

		for config := range configurations {

			logrus.Infoln("config file has changed:", config.ConfigFileUsed())

			func() {
				for {
					logrus.Infof("checking if vault is sealed...")
					sealed, err := v.Sealed()
					if err != nil {
						logrus.Errorf("error checking if vault is sealed: %s, waiting %s before trying again...", err.Error(), unsealConfig.unsealPeriod)
						time.Sleep(unsealConfig.unsealPeriod)
						continue
					}

					// If vault is sealed, we stop here and wait another unsealPeriod
					if sealed {
						logrus.Infof("vault is sealed, waiting %s before trying again...", unsealConfig.unsealPeriod)
						time.Sleep(unsealConfig.unsealPeriod)
						continue
					}

					logrus.Infof("vault is unsealed, configuring...")

					if err = v.Configure(config); err != nil {
						logrus.Errorf("error configuring vault: %s", err.Error())
						return
					}

					logrus.Infof("successfully configured vault")
					return
				}
			}()
		}
	},
}

func watchConfigurations(vaultConfigFiles []string, configurations chan *viper.Viper) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logrus.Fatal(err)
	}
	defer watcher.Close()

	for _, vaultConfigFile := range vaultConfigFiles {
		// we have to watch the entire directory to pick up renames/atomic saves in a cross-platform way
		configFile := filepath.Clean(vaultConfigFile)
		configDir, _ := filepath.Split(configFile)

		done := make(chan bool)
		go func() {
			for {
				select {
				case event := <-watcher.Events:
					// we only care about the config file or the ConfigMap directory (if in Kubernetes)
					if filepath.Clean(event.Name) == configFile || filepath.Base(event.Name) == "..data" {
						if event.Op&fsnotify.Write == fsnotify.Write || event.Op&fsnotify.Create == fsnotify.Create {
							configurations <- parseConfiguration(configFile)
						}
					}
				case err := <-watcher.Errors:
					logrus.Error(err)
				}
			}
		}()

		watcher.Add(configDir)
		<-done
	}
}

func parseConfiguration(vaultConfigFile string) *viper.Viper {

	config := viper.New()

	templateName := filepath.Base(vaultConfigFile)

	configTemplate, err := template.New(templateName).
		Funcs(sprig.TxtFuncMap()).
		Delims("${", "}").
		ParseFiles(vaultConfigFile)

	if err != nil {
		logrus.Fatalf("error parsing vault config template: %s", err.Error())
	}

	buffer := bytes.NewBuffer(nil)

	err = configTemplate.ExecuteTemplate(buffer, templateName, nil)
	if err != nil {
		logrus.Fatalf("error executing vault config template: %s", err.Error())
	}

	config.SetConfigFile(vaultConfigFile)

	err = config.ReadConfig(buffer)
	if err != nil {
		logrus.Fatalf("error reading vault config file: %s", err.Error())
	}

	return config
}

func init() {
	configureCmd.PersistentFlags().Bool(cfgOnce, false, "Run configure only once")
	configureCmd.PersistentFlags().Duration(cfgUnsealPeriod, time.Second*30, "How often to attempt to unseal the Vault instance")
	configureCmd.PersistentFlags().StringSlice(cfgVaultConfigFile, []string{vault.DefaultConfigFile}, "The filename of the YAML/JSON Vault configuration")

	rootCmd.AddCommand(configureCmd)
}
