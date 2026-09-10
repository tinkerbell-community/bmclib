// Package intelamt implements a bmclib provider for Intel AMT managed devices.
package intelamt

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bmc-toolbox/common"
	"github.com/go-logr/logr"
	"github.com/jacobweinstock/iamt"
	"github.com/jacobweinstock/registrar"

	"github.com/bmc-toolbox/bmclib/v2/providers"
)

const (
	// ProviderName for the provider AMT implementation
	ProviderName = "IntelAMT"
	// ProviderProtocol for the provider AMT implementation
	ProviderProtocol = "AMT"
)

// Features implemented by the AMT provider
var Features = registrar.Features{
	providers.FeaturePowerSet,
	providers.FeaturePowerState,
	providers.FeatureBootDeviceSet,
	providers.FeatureInventoryRead,
	providers.FeatureVirtualMedia,
	providers.FeatureSetHTTPBootURI,
	providers.FeatureUserUpdate,
}

// ErrUnsupportedBootDevice is returned for a boot device this platform cannot
// boot.
var ErrUnsupportedBootDevice = errors.New("unsupported boot device for the AMT provider")

// iamtClient interface allows us to mock the client for testing
type iamtClient interface {
	Close(context.Context) error
	IsPoweredOn(context.Context) (bool, error)
	Open(context.Context) error
	PowerCycle(context.Context) error
	PowerOff(context.Context) error
	PowerOn(context.Context) error

	Facts(context.Context) (*iamt.Facts, error)
	Inventory(context.Context) (*iamt.Inventory, error)
	SetBootOverride(context.Context, iamt.BootTarget) error
	ClearBootOverride(context.Context) error
	InsertVirtualMedia(ctx context.Context, imageURL string, enforceSecureBoot bool) error
	EjectVirtualMedia(context.Context) error
	SetAdminPassword(ctx context.Context, username, realm, newPassword string) error
}

// Conn is a connection to a BMC via Intel AMT
type Conn struct {
	client iamtClient
	// config is a value rather than a pointer so a Conn built directly --
	// which the tests do -- has usable zero-value settings instead of
	// panicking on first use.
	config Config
}

// Option for setting optional Client values
type Option func(*Config)

// WithPort sets the port number used to connect to the BMC.
func WithPort(port uint32) Option {
	return func(c *Config) {
		c.Port = port
	}
}

// WithHostScheme sets the host scheme ("http" or "https") used to connect to the BMC.
func WithHostScheme(hostScheme string) Option {
	return func(c *Config) {
		c.HostScheme = hostScheme
	}
}

// WithLogger sets the logger used by the provider.
func WithLogger(logger logr.Logger) Option {
	return func(c *Config) {
		c.Logger = logger
	}
}

// WithTimeout bounds each AMT operation. AMT firmware is slow to wake, so the
// default is generous.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Config) {
		c.Timeout = timeout
	}
}

// WithPinnedCert pins the device's TLS certificate by its hex-encoded SHA-256
// fingerprint.
//
// AMT ships a self-signed certificate that no chain can validate, so without a
// pin a TLS connection is encrypted but not authenticated. Pinning is the only
// way to know the device on the other end is the one that was enrolled.
func WithPinnedCert(fingerprint string) Option {
	return func(c *Config) {
		c.PinnedCert = fingerprint
	}
}

// WithEnforceSecureBoot requires virtual media images to be signed.
func WithEnforceSecureBoot(enforce bool) Option {
	return func(c *Config) {
		c.EnforceSecureBoot = enforce
	}
}

// Config holds the configuration for an Intel AMT connection.
type Config struct {
	// HostScheme should be either "http" or "https".
	HostScheme string
	// Port is the port number to connect to.
	Port   uint32
	Logger logr.Logger
	// Timeout bounds each operation. Zero uses the iamt default.
	Timeout time.Duration
	// PinnedCert is the hex-encoded SHA-256 of the device's leaf certificate.
	PinnedCert string
	// EnforceSecureBoot requires virtual media images to be signed.
	EnforceSecureBoot bool
}

// New creates a new AMT connection
func New(host, user, pass string, opts ...Option) *Conn {
	defaultClient := &Config{
		HostScheme: "http",
		Port:       16992,
		Logger:     logr.Discard(),
	}
	for _, opt := range opts {
		opt(defaultClient)
	}

	iopts := []iamt.Option{
		iamt.WithLogger(defaultClient.Logger),
		iamt.WithPort(defaultClient.Port),
		iamt.WithScheme(defaultClient.HostScheme),
	}
	if defaultClient.Timeout > 0 {
		iopts = append(iopts, iamt.WithTimeout(defaultClient.Timeout))
	}
	if defaultClient.PinnedCert != "" {
		iopts = append(iopts, iamt.WithPinnedCert(defaultClient.PinnedCert))
	}

	return &Conn{
		client: iamt.NewClient(host, user, pass, iopts...),
		config: *defaultClient,
	}
}

// Name of the provider
func (c *Conn) Name() string {
	return ProviderName
}

// Open a connection to the BMC via Intel AMT.
func (c *Conn) Open(ctx context.Context) (err error) {
	return c.client.Open(ctx)
}

// Close a connection to a BMC
func (c *Conn) Close(ctx context.Context) (err error) {
	return c.client.Close(ctx)
}

// Compatible tests whether a BMC is compatible with the AMT provider
func (c *Conn) Compatible(ctx context.Context) bool {
	if err := c.client.Open(ctx); err != nil {
		return false
	}

	if _, err := c.client.IsPoweredOn(ctx); err != nil {
		return false
	}

	return true
}

// bootDevices maps bmclib boot device names onto AMT boot targets.
//
// "remote_drive" maps to UEFI HTTPS boot rather than to IDE redirection: AMT
// 11 and later use USB-R rather than IDE-R for storage redirection, and its
// data plane is undocumented, so HTTPS boot is the only remote-media path this
// provider can actually drive.
var bootDevices = map[string]iamt.BootTarget{
	"pxe":          iamt.BootPxe,
	"disk":         iamt.BootHdd,
	"hdd":          iamt.BootHdd,
	"cdrom":        iamt.BootCd,
	"bios":         iamt.BootBiosSetup,
	"remote_drive": iamt.BootUefiHTTP,
}

// BootDeviceSet sets the next boot device with options.
//
// setPersistent is not honoured and a request for it is refused rather than
// silently downgraded. AMT clears its boot parameters once they are consumed,
// so every override is inherently one-shot; accepting a persistent request
// would mean the caller discovers the truth at the second boot.
//
// efiBoot is not a separate switch on AMT: the firmware boots the device in
// whatever mode the platform is configured for, so the flag is accepted and
// ignored.
func (c *Conn) BootDeviceSet(ctx context.Context, bootDevice string, setPersistent, _ bool) (ok bool, err error) {
	if setPersistent {
		return false, fmt.Errorf("%w: AMT boot overrides are always one-shot, so a persistent override cannot be honoured", ErrUnsupportedBootDevice)
	}

	target, found := bootDevices[strings.ToLower(bootDevice)]
	if !found {
		return false, fmt.Errorf("%w: %q", ErrUnsupportedBootDevice, bootDevice)
	}

	if err := c.client.SetBootOverride(ctx, target); err != nil {
		return false, err
	}
	return true, nil
}

// PowerStateGet gets the power state of a BMC machine
func (c *Conn) PowerStateGet(ctx context.Context) (state string, err error) {
	on, err := c.client.IsPoweredOn(ctx)
	if err != nil {
		return "", err
	}
	if on {
		return "on", nil
	}

	return "off", nil
}

// PowerSet sets the power state of a BMC machine
func (c *Conn) PowerSet(ctx context.Context, state string) (ok bool, err error) {
	on, _ := c.client.IsPoweredOn(ctx)

	switch strings.ToLower(state) {
	case "on":
		if on {
			return true, nil
		}
		if err := c.client.PowerOn(ctx); err != nil {
			return false, err
		}
		ok = true
	case "off":
		if !on {
			return true, nil
		}
		if err := c.client.PowerOff(ctx); err != nil {
			return false, err
		}
		ok = true
	case "cycle":
		if err := c.client.PowerCycle(ctx); err != nil {
			return false, err
		}
		ok = true
	default:
		err = errors.New("requested state type unknown")
	}

	return ok, err
}

// SetVirtualMedia mounts an image for the next boot.
//
// AMT fetches the image itself over HTTPS -- it is not streamed from here --
// so mediaURL must be reachable from the device and served with a certificate
// the device trusts. Neither can be checked from this side: a device that
// distrusts the server boots normally and reports no error.
//
// An empty mediaURL ejects, matching how other bmclib providers signal
// removal.
func (c *Conn) SetVirtualMedia(ctx context.Context, kind, mediaURL string) (ok bool, err error) {
	if mediaURL == "" {
		if err := c.client.EjectVirtualMedia(ctx); err != nil {
			return false, err
		}
		return true, nil
	}

	switch strings.ToLower(kind) {
	case "", "cd", "cdrom", "dvd":
	default:
		return false, fmt.Errorf("%w: AMT supports optical media only, got %q", ErrUnsupportedBootDevice, kind)
	}

	if err := c.client.InsertVirtualMedia(ctx, mediaURL, c.config.EnforceSecureBoot); err != nil {
		return false, err
	}
	return true, nil
}

// SetHTTPBootURI sets the URI the device boots from over UEFI HTTPS.
//
// This is the same firmware mechanism as SetVirtualMedia; it is exposed under
// both names because callers reach for whichever matches their intent.
func (c *Conn) SetHTTPBootURI(ctx context.Context, uri string) (ok bool, err error) {
	if err := c.client.InsertVirtualMedia(ctx, uri, c.config.EnforceSecureBoot); err != nil {
		return false, err
	}
	return true, nil
}

// UserUpdate changes an account's password.
//
// Only the admin account is supported: AMT's other accounts are managed
// through user ACL entries, which is a different operation from setting the
// admin credential, and role is therefore ignored.
//
// The change is not reversible and has no confirmation step. The moment it
// succeeds the old password stops working, so a caller that cannot verify the
// new password afterwards has no way to tell whether the write landed.
func (c *Conn) UserUpdate(ctx context.Context, user, pass, _ string) (ok bool, err error) {
	facts, err := c.client.Facts(ctx)
	if err != nil {
		return false, fmt.Errorf("reading the digest realm needed to set a password: %w", err)
	}
	if facts.DigestRealm == "" {
		return false, errors.New("device reported no digest realm; the password cannot be set")
	}

	if err := c.client.SetAdminPassword(ctx, user, facts.DigestRealm, pass); err != nil {
		return false, err
	}
	return true, nil
}

// Inventory collects hardware inventory over CIM.
//
// AMT reports considerably less than a server BMC: there is no model or serial
// number for storage devices, and no firmware inventory beyond AMT's own
// version. Fields the device cannot fill are left empty rather than guessed.
func (c *Conn) Inventory(ctx context.Context) (device *common.Device, err error) {
	inv, err := c.client.Inventory(ctx)
	if err != nil {
		return nil, err
	}

	dev := common.NewDevice()
	dev.Vendor = inv.Manufacturer
	dev.Model = inv.Model
	dev.Serial = inv.SerialNumber

	if inv.Baseboard.Model != "" || inv.Baseboard.SerialNumber != "" {
		dev.Mainboard = &common.Mainboard{
			Common: common.Common{
				Vendor:      inv.Baseboard.Manufacturer,
				Model:       inv.Baseboard.Model,
				Serial:      inv.Baseboard.SerialNumber,
				Description: inv.Baseboard.Version,
			},
		}
	}

	if inv.BIOS.Version != "" {
		dev.BIOS = &common.BIOS{
			Common: common.Common{
				Vendor:   inv.BIOS.Vendor,
				Firmware: &common.Firmware{Installed: inv.BIOS.Version},
			},
		}
	}

	for _, cpu := range inv.CPUs {
		// AMT's CIM_Processor DeviceID ("CPU 0") is both the processor's
		// identifier and its socket designation, and is the only thing
		// distinguishing one package from another, since AMT reports no
		// per-package serial. Consumers key on ID or Slot.
		dev.CPUs = append(dev.CPUs, &common.CPU{
			Common: common.Common{
				Description: cpu.Name,
				Model:       cpu.Name,
			},
			ID:           cpu.ID,
			Slot:         cpu.ID,
			ClockSpeedHz: int64(cpu.MaxClockMHz) * 1_000_000,
		})
	}

	for _, mod := range inv.Memory {
		dev.Memory = append(dev.Memory, &common.Memory{
			Common: common.Common{
				Vendor:      mod.Manufacturer,
				Serial:      mod.SerialNumber,
				Description: mod.BankLabel,
			},
			Slot:         mod.BankLabel,
			PartNumber:   mod.PartNumber,
			SizeBytes:    mod.CapacityBytes,
			ClockSpeedHz: int64(mod.ClockMHz) * 1_000_000,
		})
	}

	// AMT models each interface as a single CIM_EthernetPort, so the adapter
	// and its one port are the same object and carry the same name. The name
	// is repeated onto the port because consumers read the port's identifier,
	// not the adapter's, when labelling a MAC.
	for _, nic := range inv.NICs {
		dev.NICs = append(dev.NICs, &common.NIC{
			Common: common.Common{Description: nic.Name},
			ID:     nic.Name,
			NICPorts: []*common.NICPort{{
				ID:         nic.Name,
				MacAddress: nic.MACAddress,
			}},
		})
	}

	// Model and serial come from the storage package AMT reports alongside
	// each media access device, and are empty when it reports none.
	for _, drive := range inv.Drives {
		dev.Drives = append(dev.Drives, &common.Drive{
			Common: common.Common{
				Description: drive.ID,
				Model:       drive.Model,
				Serial:      drive.SerialNumber,
			},
			ID:            drive.ID,
			CapacityBytes: int64(drive.MaxMediaSizeKB) * 1024, //nolint:gosec // CIM reports kilobytes
		})
	}

	// AMT's own firmware version belongs on the BMC, not the host.
	if facts, err := c.client.Facts(ctx); err == nil && facts.Firmware.AMT != "" {
		dev.BMC = &common.BMC{
			Common: common.Common{
				Vendor:   "Intel",
				Model:    "Intel AMT",
				Firmware: &common.Firmware{Installed: facts.Firmware.AMT},
			},
		}
	}

	return &dev, nil
}
