package redfishwrapper

import (
	"context"

	"github.com/pkg/errors"
	"github.com/stmcginnis/gofish/schemas"

	"github.com/bmc-toolbox/bmclib/v2/bmc"
	bmclibErrs "github.com/bmc-toolbox/bmclib/v2/errors"
)

type bootDeviceMapping struct {
	BootDeviceType bmc.BootDeviceType
	RedFishTarget  schemas.BootSource
}

var bootDeviceTypeMappings = []bootDeviceMapping{
	{
		BootDeviceType: bmc.BootDeviceTypeBIOS,
		RedFishTarget:  schemas.BiosSetupBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypeCDROM,
		RedFishTarget:  schemas.CdBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypeDiag,
		RedFishTarget:  schemas.DiagsBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypeFloppy,
		RedFishTarget:  schemas.FloppyBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypeDisk,
		RedFishTarget:  schemas.HddBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypeNone,
		RedFishTarget:  schemas.NoneBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypePXE,
		RedFishTarget:  schemas.PxeBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypeRemoteDrive,
		RedFishTarget:  schemas.RemoteDriveBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypeSDCard,
		RedFishTarget:  schemas.SDCardBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypeUSB,
		RedFishTarget:  schemas.UsbBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceTypeUtil,
		RedFishTarget:  schemas.UtilitiesBootSource,
	},
	{
		BootDeviceType: bmc.BootDeviceUefiHTTP,
		RedFishTarget:  schemas.UefiHTTPBootSource,
	},
}

// bootDeviceStringToTarget gets the RedFish BootSource that corresponds to the given device string,
// or an error if the device is not a RedFish BootSource.
func bootDeviceStringToTarget(device string) (schemas.BootSource, error) {
	for _, bootDevice := range bootDeviceTypeMappings {
		if string(bootDevice.BootDeviceType) == device {
			return bootDevice.RedFishTarget, nil
		}
	}
	return "", errors.New("invalid boot device")
}

// bootTargetToBootDeviceType converts the redfish boot target to a bmc.BootDeviceType.
// if the target is unknown or unsupported, then an error is returned.
func bootTargetToBootDeviceType(target schemas.BootSource) (bmc.BootDeviceType, error) {
	for _, bootDevice := range bootDeviceTypeMappings {
		if bootDevice.RedFishTarget == target {
			return bootDevice.BootDeviceType, nil
		}
	}
	return "", errors.New("invalid boot device")
}

// SystemBootDeviceSet sets the boot override of the system: its target, once or
// continuous, and its firmware mode, UEFI with efiBoot and Legacy without.
//
// Only those properties are sent. The Boot object read from the BMC also holds
// read-only annotations, such as BootSourceOverrideTarget@Redfish.AllowableValues,
// and properties some BMCs do not accept back as they report them: OpenBMC on an
// ASRock Rack X570D4I-2T rejects a PATCH of the whole object with 400 for both.
func (c *Client) SystemBootDeviceSet(_ context.Context, bootDevice string, setPersistent, efiBoot bool) (ok bool, err error) {
	if err := c.SessionActive(); err != nil {
		return false, errors.Wrap(bmclibErrs.ErrNotAuthenticated, err.Error())
	}

	system, err := c.System()
	if err != nil {
		return false, err
	}

	target, err := bootDeviceStringToTarget(bootDevice)
	if err != nil {
		return false, err
	}

	enabled := schemas.OnceBootSourceOverrideEnabled
	if setPersistent {
		enabled = schemas.ContinuousBootSourceOverrideEnabled
	}

	mode := schemas.LegacyBootSourceOverrideMode
	if efiBoot {
		mode = schemas.UEFIBootSourceOverrideMode
	}

	boot := schemas.Boot{BootSourceOverrideTarget: target, BootSourceOverrideEnabled: enabled}
	// A BMC that reports no boot mode has none to set.
	if system.Boot.BootSourceOverrideMode != "" {
		boot.BootSourceOverrideMode = mode
	}

	firstErr := system.SetBoot(&boot)
	if firstErr == nil {
		return true, nil
	}
	if boot.BootSourceOverrideMode == "" {
		return false, firstErr
	}

	// Some BMCs reject BootSourceOverrideMode, or the mode asked for: a UEFI-only
	// BMC rejects Legacy, and HP DL160 Gen10 accepts only the target and enabled.
	// Set the boot device without it.
	if err := system.SetBoot(&schemas.Boot{BootSourceOverrideTarget: target, BootSourceOverrideEnabled: enabled}); err != nil {
		return false, err
	}

	// The BMC kept the mode it had. A UEFI boot in another mode fails or boots
	// something else, so do not report success when it is not UEFI.
	if efiBoot {
		system, err := c.System()
		if err != nil {
			return false, err
		}

		if kept := system.Boot.BootSourceOverrideMode; kept != "" && kept != schemas.UEFIBootSourceOverrideMode {
			return false, errors.Wrapf(firstErr, "boot device set, but the BMC refused boot mode %s and kept %s", mode, kept)
		}
	}

	return true, nil
}

// GetBootDeviceOverride returns the current boot override settings
func (c *Client) GetBootDeviceOverride(_ context.Context) (override bmc.BootDeviceOverride, err error) {
	if err := c.SessionActive(); err != nil {
		return override, errors.Wrap(bmclibErrs.ErrNotAuthenticated, err.Error())
	}

	system, err := c.System()
	if err != nil {
		return override, err
	}

	boot := system.Boot
	bootDevice, err := bootTargetToBootDeviceType(boot.BootSourceOverrideTarget)
	if err != nil {
		return override, err
	}

	override = bmc.BootDeviceOverride{
		IsPersistent: boot.BootSourceOverrideEnabled == schemas.ContinuousBootSourceOverrideEnabled,
		IsEFIBoot:    boot.BootSourceOverrideMode == schemas.UEFIBootSourceOverrideMode,
		Device:       bootDevice,
	}

	return override, nil
}
