/*
 * This file is part of arduino-cli.
 *
 * arduino-cli is free software; you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation; either version 2 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program; if not, write to the Free Software
 * Foundation, Inc., 51 Franklin St, Fifth Floor, Boston, MA  02110-1301  USA
 *
 * As a special exception, you may use this file as part of a free software
 * library without restriction.  Specifically, if other files instantiate
 * templates or use macros or inline functions from this file, or you compile
 * this file and link it with other files to produce an executable, this
 * file does not by itself cause the resulting executable to be covered by
 * the GNU General Public License.  This exception does not however
 * invalidate any other reasons why the executable file might be covered by
 * the GNU General Public License.
 *
 * Copyright 2017-2018 ARDUINO AG (http://www.arduino.cc/)
 */

package upload

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	properties "github.com/arduino/go-properties-map"
	"github.com/bcmi-labs/arduino-cli/commands"
	"github.com/bcmi-labs/arduino-cli/common/formatter"
	"github.com/bcmi-labs/arduino-cli/cores"
	"github.com/bcmi-labs/arduino-cli/executils"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	serial "go.bug.st/serial.v1"
)

// Init prepares the command.
func Init(rootCommand *cobra.Command) {
	rootCommand.AddCommand(command)
	command.Flags().StringVarP(&flags.fqbn, "fqbn", "b", "", "Fully Qualified Board Name, e.g.: arduino:avr:uno")
	command.Flags().StringVarP(&flags.port, "port", "p", "", "Upload port, e.g.: COM10 or /dev/ttyACM0")
	command.Flags().BoolVarP(&flags.verbose, "verify", "t", false, "Verify uploaded binary after the upload.")
	command.Flags().BoolVarP(&flags.verbose, "verbose", "v", false, "Optional, turns on verbose mode.")
}

var flags struct {
	fqbn    string
	port    string
	verbose bool
	verify  bool
}

var command = &cobra.Command{
	Use:     "upload",
	Short:   "Upload Arduino sketches.",
	Long:    "Upload Arduino sketches.",
	Example: "arduino upload [sketchPath]",
	Args:    cobra.MaximumNArgs(1),
	Run:     run,
}

func run(command *cobra.Command, args []string) {
	sketchPath := ""
	if len(args) > 0 {
		sketchPath = args[0]
	}
	sketch, err := commands.InitSketch(sketchPath)
	if err != nil {
		formatter.PrintError(err, "Error opening sketch.")
		os.Exit(commands.ErrGeneric)
	}

	// FIXME: make a specification on how a port is specified via command line
	port := flags.port
	if port == "" {
		formatter.PrintErrorMessage("No port provided.")
		os.Exit(commands.ErrBadCall)
	}

	fqbn := flags.fqbn
	if fqbn == "" && sketch != nil {
		fqbn = sketch.Metadata.CPU.Fqbn
	}
	if fqbn == "" {
		formatter.PrintErrorMessage("No Fully Qualified Board Name provided.")
		os.Exit(commands.ErrBadCall)
	}
	fqbnParts := strings.Split(fqbn, ":")
	if len(fqbnParts) < 3 || len(fqbnParts) > 4 {
		formatter.PrintErrorMessage("Fully Qualified Board Name has incorrect format.")
		os.Exit(commands.ErrBadCall)
	}
	packageName := fqbnParts[0]
	coreName := fqbnParts[1]
	boardName := fqbnParts[2]

	pm := commands.InitPackageManager()
	if err := pm.LoadHardware(); err != nil {
		fmt.Printf("Error loading hardware: %s", err)
		os.Exit(commands.ErrCoreConfig)
	}

	// Find target board
	// TODO: Make a packagemanager function to do this
	targetPackage := pm.GetPackages().Packages[packageName]
	if targetPackage == nil {
		formatter.PrintErrorMessage("Unknown package " + packageName + ".")
		os.Exit(commands.ErrBadCall)
	}
	platform := targetPackage.Platforms[coreName]
	if platform == nil {
		formatter.PrintErrorMessage("Unknown platform " + packageName + ":" + coreName + ".")
		os.Exit(commands.ErrBadCall)
	}
	platformRelease := platform.GetInstalled()
	if platformRelease == nil {
		formatter.PrintErrorMessage("Platform " + packageName + ":" + coreName + " is not installed.")
		os.Exit(commands.ErrBadCall)
	}
	board := platformRelease.Boards[boardName]
	if board == nil {
		formatter.PrintErrorMessage("Unknown board " + packageName + ":" + coreName + ":" + boardName + ".")
		os.Exit(commands.ErrBadCall)
	}

	// Create board configuration
	var boardProperties properties.Map
	if len(fqbnParts) == 3 {
		boardProperties = board.Properties
	} else {
		if props, err := board.GeneratePropertiesForConfiguration(fqbnParts[3]); err != nil {
			formatter.PrintError(err, "Invalid FQBN.")
			os.Exit(commands.ErrBadCall)
		} else {
			boardProperties = props
		}
	}

	// Load programmer tool
	uploadToolID, have := boardProperties["upload.tool"]
	if !have || uploadToolID == "" {
		formatter.PrintErrorMessage("The board defines an invalid 'upload.tool': " + uploadToolID)
		os.Exit(commands.ErrGeneric)
	}

	var referencedPackage *cores.Package
	var referencedPlatform *cores.Platform
	var referencedPlatformRelease *cores.PlatformRelease
	var uploadTool *cores.Tool
	if split := strings.Split(uploadToolID, ":"); len(split) == 1 {
		uploadTool = targetPackage.Tools[uploadToolID]
	} else if len(split) == 2 {
		referencedPackage = pm.GetPackages().Packages[split[0]]
		if referencedPackage == nil {
			formatter.PrintErrorMessage("The board requires a tool from package '" + split[0] + "' that is not installed: " + uploadToolID)
			os.Exit(commands.ErrGeneric)
		}
		uploadTool = referencedPackage.Tools[split[1]]

		referencedPlatform = referencedPackage.Platforms[coreName]
		if referencedPlatform != nil {
			referencedPlatformRelease = referencedPlatform.GetInstalled()
		}
	} else {
		formatter.PrintErrorMessage("The board defines an invalid 'upload.tool': " + uploadToolID)
		os.Exit(commands.ErrGeneric)
	}
	if uploadTool == nil {
		formatter.PrintErrorMessage("Upload tool '" + uploadToolID + "' not found.")
		os.Exit(commands.ErrGeneric)
	}
	// FIXME: Look into index if the platform requires a specific version
	uploadToolRelease := uploadTool.GetLatestInstalled()
	if uploadToolRelease == nil {
		formatter.PrintErrorMessage("Upload tool '" + uploadToolID + "' not installed.")
		os.Exit(commands.ErrGeneric)
	}

	// Build configuration for upload
	uploadProperties := properties.Map{}
	if referencedPlatformRelease != nil {
		uploadProperties.Merge(referencedPlatformRelease.Properties)
	}
	uploadProperties.Merge(platformRelease.Properties)
	uploadProperties.Merge(platformRelease.RuntimeProperties())
	uploadProperties.Merge(boardProperties)

	uploadToolProperties := uploadProperties.SubTree("tools." + uploadTool.Name)
	uploadProperties.Merge(uploadToolProperties)

	if requiredTools, err := pm.FindToolsRequiredForBoard(board); err == nil {
		for _, requiredTool := range requiredTools {
			uploadProperties.Merge(requiredTool.RuntimeProperties())
		}
	}

	// Set properties for verbose upload
	if flags.verbose {
		if v, ok := uploadProperties["upload.params.verbose"]; ok {
			uploadProperties["upload.verbose"] = v
		}
	} else {
		if v, ok := uploadProperties["upload.params.quiet"]; ok {
			uploadProperties["upload.verbose"] = v
		}
	}

	// Set properties for verify
	if flags.verify {
		uploadProperties["upload.verify"] = uploadProperties["upload.params.verify"]
	} else {
		uploadProperties["upload.verify"] = uploadProperties["upload.params.noverify"]
	}

	// Set path to compiled binary
	// FIXME: refactor this should be made into a function
	fqbn = strings.Replace(fqbn, ":", ".", -1)
	uploadProperties["build.path"] = sketch.FullPath
	uploadProperties["build.project_name"] = sketch.Name + "." + fqbn
	ext := filepath.Ext(uploadProperties.ExpandPropsInString("{recipe.output.tmp_file}"))
	if _, err := os.Stat(filepath.Join(sketch.FullPath, sketch.Name+"."+fqbn+ext)); err != nil {
		if os.IsNotExist(err) {
			formatter.PrintErrorMessage("Compiled sketch not found. Please compile first.")
		} else {
			formatter.PrintError(err, "Could not open compiled sketch.")
		}
		os.Exit(commands.ErrGeneric)
	}

	// Perform reset via 1200bps touch if requested
	if uploadProperties.GetBoolean("upload.use_1200bps_touch") {
		ports, err := serial.GetPortsList()
		if err != nil {
			formatter.PrintError(err, "Can't get serial port list")
			os.Exit(commands.ErrGeneric)
		}
		for _, p := range ports {
			if p == port {
				if err := touchSerialPortAt1200bps(p); err != nil {
					formatter.PrintError(err, "Can't perform reset via 1200bps-touch on serial port")
					os.Exit(commands.ErrGeneric)
				}
				break
			}
		}

		// Scanning for available ports seems to open the port or
		// otherwise assert DTR, which would cancel the WDT reset if
		// it happened within 250 ms. So we wait until the reset should
		// have already occurred before we start scanning.
		time.Sleep(500 * time.Millisecond)
	}

	// Wait for upload port if requested
	actualPort := port // default
	if uploadProperties.GetBoolean("upload.wait_for_upload_port") {
		if p, err := waitForNewSerialPort(); err != nil {
			formatter.PrintError(err, "Could not detect serial ports")
			os.Exit(commands.ErrGeneric)
		} else if p == "" {
			formatter.Print("No new serial port detected.")
		} else {
			actualPort = p
		}

		// on OS X, if the port is opened too quickly after it is detected,
		// a "Resource busy" error occurs, add a delay to workaround.
		// This apply to other platforms as well.
		time.Sleep(500 * time.Millisecond)
	}

	// Set serial port property
	uploadProperties["serial.port"] = actualPort
	if strings.HasPrefix(actualPort, "/dev/") {
		uploadProperties["serial.port.file"] = actualPort[5:]
	} else {
		uploadProperties["serial.port.file"] = actualPort
	}

	// Build recipe for upload
	recipe := uploadProperties["upload.pattern"]
	cmdLine := uploadProperties.ExpandPropsInString(recipe)
	cmdArgs, err := properties.SplitQuotedString(cmdLine, `"'`, false)
	if err != nil {
		formatter.PrintError(err, "Invalid recipe in platform.")
		os.Exit(commands.ErrCoreConfig)
	}

	// Run Tool
	cmd, err := executils.Command(cmdArgs)
	if err != nil {
		formatter.PrintError(err, "Could not execute upload tool.")
		os.Exit(commands.ErrGeneric)
	}

	executils.AttachStdoutListener(cmd, executils.PrintToStdout)
	executils.AttachStderrListener(cmd, executils.PrintToStderr)

	if err := cmd.Start(); err != nil {
		formatter.PrintError(err, "Could not execute upload tool.")
		os.Exit(commands.ErrGeneric)
	}
	if err := cmd.Wait(); err != nil {
		formatter.PrintError(err, "Error during upload.")
		os.Exit(commands.ErrGeneric)
	}
}

func touchSerialPortAt1200bps(port string) error {
	logrus.Infof("Touching port %s at 1200bps", port)

	// Open port
	p, err := serial.Open(port, &serial.Mode{BaudRate: 1200})
	if err != nil {
		return fmt.Errorf("open port: %s", err)
	}
	defer p.Close()

	if err = p.SetDTR(false); err != nil {
		return fmt.Errorf("can't set DTR")
	}
	return nil
}

// waitForNewSerialPort is meant to be called just after a reset. It watches the ports connected
// to the machine until a port appears. The new appeared port is returned
func waitForNewSerialPort() (string, error) {
	logrus.Infof("Waiting for upload port...")

	getPortMap := func() (map[string]bool, error) {
		ports, err := serial.GetPortsList()
		if err != nil {
			return nil, err
		}
		res := map[string]bool{}
		for _, port := range ports {
			res[port] = true
		}
		return res, nil
	}

	last, err := getPortMap()
	if err != nil {
		return "", fmt.Errorf("scanning serial port: %s", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		now, err := getPortMap()
		if err != nil {
			return "", fmt.Errorf("scanning serial port: %s", err)
		}

		for p := range now {
			if !last[p] {
				return p, nil // Found it!
			}
		}

		last = now
		time.Sleep(250 * time.Millisecond)
	}

	return "", nil
}