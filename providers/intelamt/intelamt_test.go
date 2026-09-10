package intelamt

import (
	"context"
	"errors"
	"testing"

	"github.com/bmc-toolbox/common"
	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/jacobweinstock/iamt"

	"github.com/bmc-toolbox/bmclib/v2/providers"
)

type mock struct {
	errSetPXE      error
	errIsPoweredOn error
	poweredON      bool
	errPowerOn     error
	errPowerOff    error
	errPowerCycle  error
	errOpen        error

	facts     *iamt.Facts
	errFacts  error
	inventory *iamt.Inventory
	errInv    error

	errBootOverride error
	errInsertMedia  error
	errSetPassword  error

	// Recorded calls, so tests can assert what actually reached the device.
	bootTargets   []iamt.BootTarget
	clearedBoot   int
	insertedMedia []string
	insertSecure  []bool
	ejectedMedia  int
	setPasswords  [][3]string
}

func (m *mock) Open(ctx context.Context) error {
	return m.errOpen
}

func (m *mock) Close(ctx context.Context) error {
	return nil
}

func (m *mock) IsPoweredOn(ctx context.Context) (bool, error) {
	if m.errIsPoweredOn != nil {
		return false, m.errIsPoweredOn
	}
	return m.poweredON, nil
}

func (m *mock) PowerOn(ctx context.Context) error {
	return m.errPowerOn
}

func (m *mock) PowerOff(ctx context.Context) error {
	return m.errPowerOff
}

func (m *mock) PowerCycle(ctx context.Context) error {
	return m.errPowerCycle
}

func (m *mock) SetPXE(ctx context.Context) error {
	return m.errSetPXE
}

func (m *mock) Facts(context.Context) (*iamt.Facts, error) {
	if m.errFacts != nil {
		return nil, m.errFacts
	}
	if m.facts == nil {
		return &iamt.Facts{}, nil
	}
	return m.facts, nil
}

func (m *mock) Inventory(context.Context) (*iamt.Inventory, error) {
	if m.errInv != nil {
		return nil, m.errInv
	}
	if m.inventory == nil {
		return &iamt.Inventory{}, nil
	}
	return m.inventory, nil
}

func (m *mock) SetBootOverride(_ context.Context, target iamt.BootTarget) error {
	if m.errBootOverride != nil {
		return m.errBootOverride
	}
	m.bootTargets = append(m.bootTargets, target)
	return nil
}

func (m *mock) ClearBootOverride(context.Context) error {
	m.clearedBoot++
	return nil
}

func (m *mock) InsertVirtualMedia(_ context.Context, imageURL string, enforceSecureBoot bool) error {
	if m.errInsertMedia != nil {
		return m.errInsertMedia
	}
	m.insertedMedia = append(m.insertedMedia, imageURL)
	m.insertSecure = append(m.insertSecure, enforceSecureBoot)
	return nil
}

func (m *mock) EjectVirtualMedia(context.Context) error {
	m.ejectedMedia++
	return nil
}

func (m *mock) SetAdminPassword(_ context.Context, username, realm, newPassword string) error {
	if m.errSetPassword != nil {
		return m.errSetPassword
	}
	m.setPasswords = append(m.setPasswords, [3]string{username, realm, newPassword})
	return nil
}

func TestClose(t *testing.T) {
	conn := &Conn{client: &mock{}}
	if err := conn.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestName(t *testing.T) {
	conn := &Conn{client: &mock{}}
	if diff := cmp.Diff(conn.Name(), ProviderName); diff != "" {
		t.Fatal(diff)
	}
}

func TestBootDeviceSet(t *testing.T) {
	tests := map[string]struct {
		want     bool
		err      error
		failCall bool
		device   string
	}{
		"success":                   {want: true, device: "pxe"},
		"invalid boot device":       {want: false, err: ErrUnsupportedBootDevice, device: "invalid"},
		"failed to set boot device": {want: false, failCall: true, err: errors.New(""), device: "pxe"},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := &mock{}
			if tt.failCall {
				// The provider routes boot changes through SetBootOverride,
				// so that is where the failure has to come from.
				m = &mock{errBootOverride: tt.err}
			}
			conn := &Conn{client: m}
			ctx := context.Background()
			if err := conn.Open(ctx); err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			got, err := conn.BootDeviceSet(ctx, tt.device, false, false)
			if err != nil && tt.err == nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if diff := cmp.Diff(got, tt.want); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestPowerStateGet(t *testing.T) {
	tests := map[string]struct {
		want string
		err  error
	}{
		"power on":                  {want: "on"},
		"power off":                 {want: "off"},
		"invalid power state":       {want: "", err: errors.New("invalid power state: invalid")},
		"failed to set power state": {want: "", err: errors.New("")},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			var state bool
			switch tt.want {
			case "on":
				state = true
			case "off":
				state = false
			default:
			}
			m := &mock{poweredON: state, errIsPoweredOn: tt.err}
			conn := &Conn{client: m}
			ctx := context.Background()
			if err := conn.Open(ctx); err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			got, err := conn.PowerStateGet(ctx)
			if err != nil && tt.err == nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if diff := cmp.Diff(got, tt.want); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestPowerSet(t *testing.T) {
	tests := map[string]struct {
		want      bool
		err       error
		poweredOn bool
		wantState string
	}{
		"power on success":     {want: true, wantState: "on"},
		"power on success 2":   {want: true, wantState: "on", poweredOn: true},
		"power on failed":      {want: false, wantState: "on", err: errors.New("failed to power on")},
		"power off success":    {want: true, wantState: "off"},
		"power off success 2":  {want: true, wantState: "off", poweredOn: true},
		"power off failed":     {want: false, poweredOn: true, wantState: "off", err: errors.New("failed to power off")},
		"power cycle success":  {want: true, wantState: "cycle"},
		"power cycle failed":   {want: false, wantState: "cycle", err: errors.New("failed to power cycle")},
		"power cycle failed 2": {want: false, wantState: "cycle", poweredOn: false, err: errors.New("failed to power cycle")},
		"invalid power state":  {want: false, wantState: "unknown", err: errors.New("requested state type unknown")},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := &mock{}
			switch name {
			case "power on failed":
				m.errPowerOn = tt.err
			case "power off failed":
				m.errPowerOff = tt.err
			case "power cycle failed":
				m.errPowerCycle = tt.err
			case "power cycle failed 2":
				m.errPowerCycle = tt.err
				m.errPowerOn = tt.err
			default:
			}
			m.poweredON = tt.poweredOn
			conn := &Conn{client: m}
			ctx := context.Background()
			if err := conn.Open(ctx); err != nil {
				t.Fatal(err)
			}
			defer conn.Close(ctx)
			got, err := conn.PowerSet(ctx, tt.wantState)
			if err != nil && tt.err == nil {
				t.Fatalf("expected nil error, got: %v", err)
			}
			if diff := cmp.Diff(got, tt.want); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestCompatible(t *testing.T) {
	tests := map[string]struct {
		want       bool
		failOnOpen bool
	}{
		"success":         {want: true},
		"failed on open":  {want: false, failOnOpen: true},
		"failed on power": {want: false},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			m := &mock{}
			if !tt.want {
				if tt.failOnOpen {
					m.errOpen = errors.New("failed to open")
				} else {
					m.errIsPoweredOn = errors.New("failed to power on")
				}
			}
			conn := &Conn{client: m}
			ctx := context.Background()
			defer conn.Close(ctx)
			got := conn.Compatible(ctx)
			if diff := cmp.Diff(got, tt.want); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestNew(t *testing.T) {
	wantClient := &mock{}
	want := &Conn{client: wantClient}
	got := New("localhost", "admin", "pass")
	t.Log(got == nil)
	c := Conn{}
	l := logr.Logger{}
	if diff := cmp.Diff(got, want, cmpopts.IgnoreUnexported(c, l)); diff != "" {
		t.Fatal(diff)
	}
}

// BootDeviceSet used to accept only "pxe". The provider now maps the devices
// AMT can actually be told to boot.
func TestBootDeviceSetTargets(t *testing.T) {
	tests := map[string]struct {
		device     string
		persistent bool
		want       iamt.BootTarget
		wantErr    bool
	}{
		"pxe":                {device: "pxe", want: iamt.BootPxe},
		"disk":               {device: "disk", want: iamt.BootHdd},
		"hdd":                {device: "hdd", want: iamt.BootHdd},
		"cdrom":              {device: "cdrom", want: iamt.BootCd},
		"bios":               {device: "bios", want: iamt.BootBiosSetup},
		"remote_drive":       {device: "remote_drive", want: iamt.BootUefiHTTP},
		"case insensitive":   {device: "PXE", want: iamt.BootPxe},
		"unknown device":     {device: "floppy", wantErr: true},
		"persistent refused": {device: "pxe", persistent: true, wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			m := &mock{}
			conn := &Conn{client: m}

			ok, err := conn.BootDeviceSet(context.Background(), tc.device, tc.persistent, false)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error, got none")
				}
				if len(m.bootTargets) != 0 {
					t.Errorf("a rejected request still reached the device: %v", m.bootTargets)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !ok {
				t.Error("ok = false, want true")
			}
			if len(m.bootTargets) != 1 || m.bootTargets[0] != tc.want {
				t.Errorf("boot targets = %v, want [%v]", m.bootTargets, tc.want)
			}
		})
	}
}

// A persistent override cannot be honoured: AMT clears its boot parameters
// once consumed. Refusing beats silently downgrading, because the caller would
// otherwise discover the truth at the second boot.
func TestBootDeviceSetRefusesPersistent(t *testing.T) {
	m := &mock{}
	conn := &Conn{client: m}

	if _, err := conn.BootDeviceSet(context.Background(), "pxe", true, false); err == nil {
		t.Fatal("expected persistent to be refused")
	} else if !errors.Is(err, ErrUnsupportedBootDevice) {
		t.Errorf("error = %v, want ErrUnsupportedBootDevice", err)
	}
}

func TestSetVirtualMedia(t *testing.T) {
	t.Run("inserts an image", func(t *testing.T) {
		m := &mock{}
		conn := &Conn{client: m}

		ok, err := conn.SetVirtualMedia(context.Background(), "CD", "https://x.example.com/a.iso")
		if err != nil || !ok {
			t.Fatalf("SetVirtualMedia: ok=%v err=%v", ok, err)
		}
		if len(m.insertedMedia) != 1 || m.insertedMedia[0] != "https://x.example.com/a.iso" {
			t.Errorf("inserted = %v", m.insertedMedia)
		}
	})

	t.Run("empty URL ejects", func(t *testing.T) {
		m := &mock{}
		conn := &Conn{client: m}

		ok, err := conn.SetVirtualMedia(context.Background(), "CD", "")
		if err != nil || !ok {
			t.Fatalf("SetVirtualMedia: ok=%v err=%v", ok, err)
		}
		if m.ejectedMedia != 1 {
			t.Errorf("ejected = %d, want 1", m.ejectedMedia)
		}
		if len(m.insertedMedia) != 0 {
			t.Errorf("an eject inserted media: %v", m.insertedMedia)
		}
	})

	t.Run("non-optical kind is refused", func(t *testing.T) {
		m := &mock{}
		conn := &Conn{client: m}

		if _, err := conn.SetVirtualMedia(context.Background(), "floppy", "https://x/a.img"); err == nil {
			t.Fatal("expected floppy to be refused")
		}
		if len(m.insertedMedia) != 0 {
			t.Error("a refused kind still reached the device")
		}
	})

	t.Run("secure boot policy is passed through", func(t *testing.T) {
		m := &mock{}
		conn := &Conn{client: m, config: Config{EnforceSecureBoot: true}}

		if _, err := conn.SetVirtualMedia(context.Background(), "CD", "https://x/a.iso"); err != nil {
			t.Fatalf("SetVirtualMedia: %v", err)
		}
		if len(m.insertSecure) != 1 || !m.insertSecure[0] {
			t.Errorf("enforceSecureBoot = %v, want [true]", m.insertSecure)
		}
	})
}

// SetHTTPBootURI and SetVirtualMedia are the same firmware mechanism.
func TestSetHTTPBootURI(t *testing.T) {
	m := &mock{}
	conn := &Conn{client: m}

	ok, err := conn.SetHTTPBootURI(context.Background(), "https://x.example.com/boot.iso")
	if err != nil || !ok {
		t.Fatalf("SetHTTPBootURI: ok=%v err=%v", ok, err)
	}
	if len(m.insertedMedia) != 1 || m.insertedMedia[0] != "https://x.example.com/boot.iso" {
		t.Errorf("inserted = %v", m.insertedMedia)
	}
}

// The digest realm must come from the device: a password hashed against the
// wrong realm is accepted by AMT and then unusable.
func TestUserUpdateUsesTheDeviceRealm(t *testing.T) {
	m := &mock{facts: &iamt.Facts{DigestRealm: "Digest:AB"}}
	conn := &Conn{client: m}

	ok, err := conn.UserUpdate(context.Background(), "admin", "NewPassw0rd!", "")
	if err != nil || !ok {
		t.Fatalf("UserUpdate: ok=%v err=%v", ok, err)
	}
	if len(m.setPasswords) != 1 {
		t.Fatalf("setPasswords = %v", m.setPasswords)
	}
	if got := m.setPasswords[0]; got[0] != "admin" || got[1] != "Digest:AB" || got[2] != "NewPassw0rd!" {
		t.Errorf("SetAdminPassword got %v", got)
	}
}

func TestUserUpdateWithoutRealmIsRefused(t *testing.T) {
	m := &mock{facts: &iamt.Facts{}}
	conn := &Conn{client: m}

	if _, err := conn.UserUpdate(context.Background(), "admin", "NewPassw0rd!", ""); err == nil {
		t.Fatal("expected a missing digest realm to be refused")
	}
	if len(m.setPasswords) != 0 {
		t.Error("a password was written without a realm")
	}
}

func TestInventory(t *testing.T) {
	m := &mock{
		facts: &iamt.Facts{Firmware: iamt.Firmware{AMT: "18.1.18"}},
		inventory: &iamt.Inventory{
			Manufacturer: "ASUSTeK COMPUTER INC.",
			Model:        "NUC15CRHV7",
			SerialNumber: "TBARQK0038327AB",
			Baseboard: iamt.Baseboard{
				Manufacturer: "ASUSTeK COMPUTER INC.",
				Model:        "NUC15CRBV7",
				SerialNumber: "TBARP10006D0",
			},
			BIOS: iamt.BIOS{Vendor: "ASUSTeK COMPUTER INC.", Version: "CRARLV57"},
			CPUs: []iamt.CPU{{Name: "Managed System CPU", MaxClockMHz: 5300}},
			Memory: []iamt.MemoryModule{
				{BankLabel: "BANK 0", Manufacturer: "Corsair", CapacityBytes: 51539607552, ClockMHz: 5600},
				{BankLabel: "BANK 1", Manufacturer: "Corsair", CapacityBytes: 51539607552, ClockMHz: 5600},
			},
			NICs:   []iamt.NIC{{MACAddress: "88:ae:dd:75:3d:a0", Name: "Wired0"}},
			Drives: []iamt.Drive{{ID: "MEDIA DEV 0", MaxMediaSizeKB: 2048408248}},
		},
	}
	conn := &Conn{client: m}

	dev, err := conn.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if dev.Model != "NUC15CRHV7" || dev.Serial != "TBARQK0038327AB" {
		t.Errorf("model/serial = %q/%q", dev.Model, dev.Serial)
	}
	if dev.Mainboard == nil || dev.Mainboard.Model != "NUC15CRBV7" {
		t.Errorf("mainboard = %+v", dev.Mainboard)
	}
	if dev.BIOS == nil || dev.BIOS.Firmware == nil || dev.BIOS.Firmware.Installed != "CRARLV57" {
		t.Errorf("bios = %+v", dev.BIOS)
	}
	if len(dev.CPUs) != 1 || dev.CPUs[0].ClockSpeedHz != 5_300_000_000 {
		t.Errorf("cpus = %+v", dev.CPUs)
	}
	if len(dev.Memory) != 2 {
		t.Fatalf("memory modules = %d, want 2", len(dev.Memory))
	}
	var total int64
	for _, mod := range dev.Memory {
		total += mod.SizeBytes
	}
	if total != 103079215104 {
		t.Errorf("total memory = %d, want 103079215104", total)
	}
	if len(dev.NICs) != 1 || dev.NICs[0].NICPorts[0].MacAddress != "88:ae:dd:75:3d:a0" {
		t.Errorf("nics = %+v", dev.NICs)
	}
	// AMT's own version belongs on the BMC, not the host.
	if dev.BMC == nil || dev.BMC.Firmware == nil || dev.BMC.Firmware.Installed != "18.1.18" {
		t.Errorf("bmc = %+v", dev.BMC)
	}
	// AMT reports no model or serial for drives, so those must stay empty
	// rather than be filled with a generic element name.
	if len(dev.Drives) != 1 {
		t.Fatalf("drives = %d, want 1", len(dev.Drives))
	}
	if dev.Drives[0].Model != "" || dev.Drives[0].Serial != "" {
		t.Errorf("drive model/serial should be empty, got %q/%q", dev.Drives[0].Model, dev.Drives[0].Serial)
	}
	if dev.Drives[0].CapacityBytes != 2048408248*1024 {
		t.Errorf("drive capacity = %d", dev.Drives[0].CapacityBytes)
	}
}

// A device that reports nothing must still produce a usable Device rather than
// a nil dereference.
func TestInventoryEmptyDevice(t *testing.T) {
	conn := &Conn{client: &mock{}}

	dev, err := conn.Inventory(context.Background())
	if err != nil {
		t.Fatalf("Inventory: %v", err)
	}
	if dev == nil {
		t.Fatal("Inventory returned nil")
	}
	// common.NewDevice pre-populates these, so the assertion is that nothing
	// was invented to fill them, not that the pointers are nil.
	if dev.Mainboard != nil && dev.Mainboard.Model != "" {
		t.Errorf("mainboard model was invented: %q", dev.Mainboard.Model)
	}
	if dev.BIOS != nil && dev.BIOS.Firmware != nil && dev.BIOS.Firmware.Installed != "" {
		t.Errorf("bios version was invented: %q", dev.BIOS.Firmware.Installed)
	}
	if len(dev.CPUs) != 0 || len(dev.Memory) != 0 || len(dev.NICs) != 0 {
		t.Errorf("components were invented: cpus=%d memory=%d nics=%d",
			len(dev.CPUs), len(dev.Memory), len(dev.NICs))
	}
}

// The features the provider advertises must be the ones it implements.
func TestFeaturesAreImplemented(t *testing.T) {
	var conn any = &Conn{client: &mock{}}

	checks := map[string]bool{
		string(providers.FeatureInventoryRead):  false,
		string(providers.FeatureVirtualMedia):   false,
		string(providers.FeatureSetHTTPBootURI): false,
		string(providers.FeatureUserUpdate):     false,
	}

	if _, ok := conn.(interface {
		Inventory(context.Context) (*common.Device, error)
	}); ok {
		checks[string(providers.FeatureInventoryRead)] = true
	}
	if _, ok := conn.(interface {
		SetVirtualMedia(context.Context, string, string) (bool, error)
	}); ok {
		checks[string(providers.FeatureVirtualMedia)] = true
	}
	if _, ok := conn.(interface {
		SetHTTPBootURI(context.Context, string) (bool, error)
	}); ok {
		checks[string(providers.FeatureSetHTTPBootURI)] = true
	}
	if _, ok := conn.(interface {
		UserUpdate(context.Context, string, string, string) (bool, error)
	}); ok {
		checks[string(providers.FeatureUserUpdate)] = true
	}

	for _, f := range Features {
		if implemented, tracked := checks[string(f)]; tracked && !implemented {
			t.Errorf("feature %q is advertised but the interface is not satisfied", f)
		}
	}
	for feature, implemented := range checks {
		if !implemented {
			continue
		}
		var found bool
		for _, f := range Features {
			if string(f) == feature {
				found = true
			}
		}
		if !found {
			t.Errorf("feature %q is implemented but not advertised", feature)
		}
	}
}
