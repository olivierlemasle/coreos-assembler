// Copyright 2019 Red Hat, Inc.
//
// Run qemu directly as a subprocess.
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
	"context"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/coreos/mantle/kola"
	"github.com/coreos/mantle/platform"
	"github.com/coreos/mantle/platform/conf"
)

var (
	cmdQemuExec = &cobra.Command{
		RunE:    runQemuExec,
		PreRunE: preRun,
		Use:     "qemuexec",
		Short:   "Directly execute qemu on a CoreOS instance",

		SilenceUsage: true,
	}

	memory       int
	addDisks     []string
	usernet      bool
	cpuCountHost bool

	hostname string
	ignition string
	kargs    string
	knetargs string

	ignitionFragments []string
	bindro            []string
	bindrw            []string

	directIgnition            bool
	forceConfigInjection      bool
	propagateInitramfsFailure bool

	devshell        bool
	devshellConsole bool

	consoleFile string
)

func init() {
	root.AddCommand(cmdQemuExec)
	cmdQemuExec.Flags().StringVarP(&knetargs, "knetargs", "", "", "Arguments for Ignition networking on kernel commandline")
	cmdQemuExec.Flags().StringVarP(&kargs, "kargs", "", "", "Additional kernel arguments applied")
	cmdQemuExec.Flags().BoolVarP(&usernet, "usernet", "U", false, "Enable usermode networking")
	cmdQemuExec.Flags().StringSliceVar(&ignitionFragments, "add-ignition", nil, "Append well-known Ignition fragment: [\"autologin\"]")
	cmdQemuExec.Flags().StringVarP(&hostname, "hostname", "", "", "Set hostname via DHCP")
	cmdQemuExec.Flags().IntVarP(&memory, "memory", "m", 0, "Memory in MB")
	cmdQemuExec.Flags().StringArrayVarP(&addDisks, "add-disk", "D", []string{}, "Additional disk, human readable size (repeatable)")
	cmdQemuExec.Flags().BoolVar(&cpuCountHost, "auto-cpus", false, "Automatically set number of cpus to host count")
	cmdQemuExec.Flags().BoolVar(&directIgnition, "ignition-direct", false, "Do not parse Ignition, pass directly to instance")
	cmdQemuExec.Flags().BoolVar(&devshell, "devshell", false, "Enable development shell")
	cmdQemuExec.Flags().BoolVarP(&devshellConsole, "devshell-console", "c", false, "Connect directly to serial console in devshell mode")
	cmdQemuExec.Flags().StringVarP(&ignition, "ignition", "i", "", "Path to ignition config")
	cmdQemuExec.Flags().StringArrayVar(&bindro, "bind-ro", nil, "Mount readonly via 9pfs a host directory (use --bind-ro=/path/to/host,/var/mnt/guest")
	cmdQemuExec.Flags().StringArrayVar(&bindrw, "bind-rw", nil, "Same as above, but writable")
	cmdQemuExec.Flags().BoolVarP(&forceConfigInjection, "inject-ignition", "", false, "Force injecting Ignition config using guestfs")
	cmdQemuExec.Flags().BoolVar(&propagateInitramfsFailure, "propagate-initramfs-failure", false, "Error out if the system fails in the initramfs")
	cmdQemuExec.Flags().StringVarP(&consoleFile, "console-to-file", "", "", "Filepath in which to save serial console logs")

}

func renderFragments(fragments []string, c *conf.Conf) error {
	for _, fragtype := range fragments {
		switch fragtype {
		case "autologin":
			c.AddAutoLogin()
		default:
			return fmt.Errorf("Unknown fragment: %s", fragtype)
		}
	}
	return nil
}

func parseBindOpt(s string) (string, string, error) {
	parts := strings.SplitN(s, ",", 2)
	if len(parts) == 1 {
		return "", "", fmt.Errorf("malformed bind option, required: SRC,DEST")
	}
	return parts[0], parts[1], nil
}

func runQemuExec(cmd *cobra.Command, args []string) error {
	var err error
	var config *conf.Conf

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if devshellConsole {
		devshell = true

		if consoleFile != "" {
			return fmt.Errorf("Cannot use console devshell and --console-to-file")
		}
	}
	if devshell {
		if directIgnition {
			return fmt.Errorf("Cannot use devshell with direct ignition")
		}
		if kola.QEMUOptions.DiskImage == "" && kolaPlatform == "qemu" {
			return fmt.Errorf("No disk image provided")
		}
		ignitionFragments = append(ignitionFragments, "autologin")
		cpuCountHost = true
		usernet = true
		// Can't use 9p on RHEL8, need https://virtio-fs.gitlab.io/ instead in the future
		if kola.Options.CosaWorkdir != "" && !strings.HasPrefix(filepath.Base(kola.QEMUOptions.DiskImage), "rhcos") {
			// Conservatively bind readonly to avoid anything in the guest (stray tests, whatever)
			// from destroying stuff
			bindro = append(bindro, fmt.Sprintf("%s,/var/mnt/workdir", kola.Options.CosaWorkdir))
			// But provide the tempdir so it's easy to pass stuff back
			bindrw = append(bindrw, fmt.Sprintf("%s,/var/mnt/workdir-tmp", kola.Options.CosaWorkdir+"/tmp"))
		}
		if hostname == "" {
			hostname = devshellHostname
		}
	}

	if directIgnition && ignition == "" {
		return fmt.Errorf("Cannot use ignition-direct without a path to an Ignition config")
	}
	if !directIgnition {
		if ignition == "" {
			config, err = conf.EmptyIgnition().Render(kola.IsIgnitionV2())
			if err != nil {
				return errors.Wrapf(err, "creating empty config")
			}
		} else {
			buf, err := ioutil.ReadFile(ignition)
			if err != nil {
				return err
			}
			config, err = conf.Ignition(string(buf)).Render(kola.IsIgnitionV2())
			if err != nil {
				return errors.Wrapf(err, "parsing %s", ignition)
			}
		}
	}

	if len(ignitionFragments) > 0 {
		if directIgnition {
			return fmt.Errorf("Cannot use fragments with direct ignition")
		}
		err := renderFragments(ignitionFragments, config)
		if err != nil {
			return errors.Wrapf(err, "rendering fragments")
		}
	}

	builder := platform.NewBuilder()
	defer builder.Close()
	for _, b := range bindro {
		if directIgnition {
			return fmt.Errorf("Cannot use mounts with direct ignition")
		}
		src, dest, err := parseBindOpt(b)
		if err != nil {
			return err
		}
		builder.Mount9p(src, dest, true)
		config.Mount9p(dest, true)
	}
	for _, b := range bindrw {
		if directIgnition {
			return fmt.Errorf("Cannot use mounts with direct ignition")
		}
		src, dest, err := parseBindOpt(b)
		if err != nil {
			return err
		}
		builder.Mount9p(src, dest, false)
		config.Mount9p(dest, false)
	}
	builder.ForceConfigInjection = forceConfigInjection
	if len(knetargs) > 0 {
		builder.IgnitionNetworkKargs = knetargs
	}
	builder.AppendKernelArguments = kargs
	builder.Firmware = kola.QEMUOptions.Firmware
	if kola.QEMUOptions.DiskImage != "" {
		channel := "virtio"
		if kola.QEMUOptions.Nvme {
			channel = "nvme"
		}
		sectorSize := 0
		if kola.QEMUOptions.Native4k {
			sectorSize = 4096
		}
		err = builder.AddBootDisk(&platform.Disk{
			BackingFile:   kola.QEMUOptions.DiskImage,
			Channel:       channel,
			Size:          kola.QEMUOptions.DiskSize,
			SectorSize:    sectorSize,
			MultiPathDisk: kola.QEMUOptions.MultiPathDisk,
			NbdDisk:       kola.QEMUOptions.NbdDisk,
		})
		if err != nil {
			return err
		}
	}
	if kola.QEMUIsoOptions.IsoPath != "" {
		err := builder.AddIso(kola.QEMUIsoOptions.IsoPath, "")
		if err != nil {
			return err
		}
	}
	builder.Hostname = hostname
	if memory != 0 {
		builder.Memory = memory
	}
	for _, size := range addDisks {
		if err := builder.AddDisk(&platform.Disk{
			Size: size,
		}); err != nil {
			return errors.Wrapf(err, "adding additional disk")
		}
	}
	if cpuCountHost {
		builder.Processors = -1
	}
	if usernet {
		h := []platform.HostForwardPort{
			{Service: "ssh", HostPort: 0, GuestPort: 22},
		}
		builder.EnableUsermodeNetworking(h)
	}
	builder.InheritConsole = true
	builder.ConsoleFile = consoleFile
	builder.Append(args...)

	if devshell && !devshellConsole {
		return runDevShellSSH(ctx, builder, config)
	}
	if config != nil {
		if directIgnition {
			return fmt.Errorf("Cannot use fragments/mounts with direct ignition")
		}
		builder.SetConfig(config)
	} else if directIgnition {
		builder.ConfigFile = ignition
	}

	inst, err := builder.Exec()
	if err != nil {
		return err
	}
	defer inst.Destroy()

	if propagateInitramfsFailure {
		err := inst.WaitAll(ctx)
		if err != nil {
			return err
		}
		return nil
	}
	return inst.Wait()
}
