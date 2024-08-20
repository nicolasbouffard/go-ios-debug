package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/danielpaulus/go-ios/ios"
	"github.com/danielpaulus/go-ios/ios/tunnel"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"go.uber.org/multierr"
)

var tm *tunnel.TunnelManager

// _requestsMap stores a mutex for every request attempt to the trio formed by address, port, and TUN port. This allows to make sure that no more than one request is made at a time to a given trio, since doing so can lead to stuck requests and, therefore, stuck programs and/or goroutine leaks.
var _requestsMap = sync.Map{}

var rootCmd = &cobra.Command{
	Use:     "debug",
	Version: "1.0.0",
	Short:   "Debug go-iOS",
	RunE:    debug,
}

var (
	_userspaceTUN            bool
	_useConcurrencySafeguard bool
)

func init() {
	rootCmd.PersistentFlags().BoolVarP(&_userspaceTUN, "userspace", "u", true, "Use userspace TUN implementation")
	rootCmd.PersistentFlags().BoolVarP(&_useConcurrencySafeguard, "use-concurrency-safeguard", "s", false, "Use concurrency safeguard")
}

func main() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func debug(_ *cobra.Command, _ []string) (err error) {
	var wg, devicesWg sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	interrupt := make(chan os.Signal, 1)
	signal.Notify(
		interrupt,
		os.Interrupt,
		os.Kill,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM,
	)

	go func() {
		select {
		case <-ctx.Done():
		case <-interrupt:
		}

		cancel()
	}()

	defer func() {
		devicesWg.Wait()

		cancel()

		wg.Wait()

		if err != nil {
			panic(err)
		}
	}()

	err = runUsbmuxd(ctx, &wg)
	if err != nil {
		err = errors.Wrap(err, "run usbmuxd")
		return
	}

	err = runGoNCM(ctx, &wg)
	if err != nil {
		err = errors.Wrap(err, "run go-ncm")
		return
	}

	var devices ios.DeviceList
	devices, err = ios.ListDevices()
	if err != nil {
		err = errors.Wrap(err, "list devices")
		return
	}

	if len(devices.DeviceList) == 0 {
		err = errors.Wrap(err, "no devices")
		return
	}

	err = startTunnels(ctx, _userspaceTUN)
	if err != nil {
		err = errors.Wrap(err, "start tunnels")
		return
	}

	device := devices.DeviceList[0]

	err = useCase(device.Properties.SerialNumber)
	if err != nil {
		err = errors.Wrap(err, "run usecase")
		return
	}

	log.Println("Success")

	return
}

func runCommandAsync(ctx context.Context, wg *sync.WaitGroup, command string, args ...string) (err error) {
	var cmd *exec.Cmd
	cmd, err = buildCommand(ctx, command, args...)
	if err != nil {
		err = errors.Wrap(err, "build command")
		return
	}

	err = cmd.Start()
	if err != nil {
		err = errors.Wrap(err, "start command")
		return
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		errTmp := cmd.Wait()
		if errTmp != nil && !strings.Contains(errTmp.Error(), "signal: killed") {
			log.Println("Couldn't wait for command", errTmp)
		}
	}()

	return
}

func buildCommand(ctx context.Context, command string, args ...string) (cmd *exec.Cmd, err error) {
	cmd = exec.CommandContext(ctx, command, args...)

	err = attachLogger(cmd)
	if err != nil {
		err = errors.Wrap(err, "attach logger")
		return
	}

	return
}

func attachLogger(cmd *exec.Cmd) (err error) {
	var stdout io.ReadCloser
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		err = errors.Wrap(err, "pipe stdout")
		return
	}

	var stderr io.ReadCloser
	stderr, err = cmd.StderrPipe()
	if err != nil {
		err = errors.Wrap(err, "pipe stderr")
		return
	}

	stdoutScanner := bufio.NewScanner(stdout)
	stderrScanner := bufio.NewScanner(stderr)

	go func() {
		for stdoutScanner.Scan() {
			log.Println(stdoutScanner.Text())
		}

		if err = stdoutScanner.Err(); err != nil {
		}
	}()

	go func() {
		for stderrScanner.Scan() {
			log.Println(stderrScanner.Text())
		}

		if err = stderrScanner.Err(); err != nil {
		}
	}()

	return
}

func runUsbmuxd(ctx context.Context, wg *sync.WaitGroup) (err error) {
	err = runCommandAsync(ctx, wg, "usbmuxd", "-f", "-v")
	if err != nil {
		err = errors.Wrap(err, "run command async")
		return
	}

	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			err = errors.Wrap(err, "retry getting usbmuxd status")
			return
		case <-ticker.C:
			_, errTmp := ios.ListDevices()
			if errTmp != nil {
				multierr.AppendInto(&err, errors.Wrap(errTmp, "list devices"))
				continue
			}

			err = nil

			return
		}
	}
}

func runGoNCM(ctx context.Context, wg *sync.WaitGroup) (err error) {
	err = runCommandAsync(ctx, wg, "./go-ncm")
	if err != nil {
		err = errors.Wrap(err, "run command async")
		return
	}

	time.Sleep(10 * time.Second)

	return
}

func startTunnels(ctx context.Context, userspaceTUN bool) (err error) {
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	var pm tunnel.PairRecordManager
	pm, err = tunnel.NewPairRecordManager(".")
	if err != nil {
		err = errors.Wrap(err, "create pair record manager")
		return
	}

	tm = tunnel.NewTunnelManager(pm, userspaceTUN)

	err = tm.UpdateTunnels(ctx)
	if err != nil {
		err = errors.Wrap(err, "update tunnels once")
		return
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			multierr.AppendInto(&err, ctx.Err())
			return
		case <-ticker.C:
			if !tm.FirstUpdateCompleted() {
				multierr.AppendInto(&err, errors.New("first update not completed"))
				continue
			}

			err = nil

			return
		}
	}
}

func useCase(udid string) (err error) {
	var wg sync.WaitGroup
	defer wg.Wait()

	var mu sync.Mutex

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			_, errTmp := getDevice(udid)
			if errTmp != nil {
				mu.Lock()
				multierr.AppendInto(&err, errors.Wrapf(errTmp, "get device"))
				mu.Unlock()
				return
			}

		}(i)
	}

	return
}

func getDevice(udid string) (device ios.DeviceEntry, err error) {
	var deviceTmp ios.DeviceEntry
	deviceTmp, err = ios.GetDevice(udid)
	if err != nil {
		err = errors.Wrap(err, "get device")
		return
	}

	var t tunnel.Tunnel
	t, err = tm.FindTunnel(udid)
	if err != nil {
		err = errors.Wrap(err, "find tunnel")
		return
	}

	if t.Udid == "" {
		err = errors.New("tunnel not found")
		return
	}

	deviceTmp.UserspaceTUNPort = t.UserspaceTUNPort
	deviceTmp.UserspaceTUN = t.UserspaceTUN

	var rsdService ios.RsdService

	if _useConcurrencySafeguard {
		rsdService, err = NewWithAddrPortWithConcurrencySafeguard(t.Address, t.RsdPort, deviceTmp)
	} else {
		rsdService, err = ios.NewWithAddrPort(t.Address, t.RsdPort, deviceTmp)
	}

	if err != nil {
		err = errors.Wrap(err, "new with addr port")
		return
	}

	defer rsdService.Close()
	var rsdProvider ios.RsdHandshakeResponse
	rsdProvider, err = rsdService.Handshake()
	if err != nil {
		err = errors.Wrap(err, "handshake")
		return
	}

	device, err = ios.GetDeviceWithAddress(udid, t.Address, rsdProvider)
	if err != nil {
		err = errors.Wrap(err, "get device with address")
		return
	}

	device.UserspaceTUN = deviceTmp.UserspaceTUN
	device.UserspaceTUNPort = deviceTmp.UserspaceTUNPort

	return
}

func NewWithAddrPortWithConcurrencySafeguard(addr string, port int, d ios.DeviceEntry) (rsdService ios.RsdService, err error) {
	key := fmt.Sprintf("%s-%d-%d", addr, port, d.UserspaceTUNPort)

	mutex, _ := _requestsMap.LoadOrStore(key, &sync.Mutex{})

	mutex.(*sync.Mutex).Lock()
	defer mutex.(*sync.Mutex).Unlock()

	rsdService, err = ios.NewWithAddrPort(addr, port, d)
	err = errors.WithStack(err)

	return
}
