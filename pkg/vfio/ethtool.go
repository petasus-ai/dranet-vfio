/*
Copyright The Kubernetes Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vfio

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/mdlayher/genetlink"
	mdnetlink "github.com/mdlayher/netlink"
	"golang.org/x/sys/unix"
)

// EthtoolOps reads a netdev's link over ethtool netlink
// (https://docs.kernel.org/networking/ethtool-netlink.html).
type EthtoolOps interface {
	// GetLink returns what could be read, with the errors of the reads that
	// failed.
	GetLink(ifName string) (EthtoolLink, error)
}

// EthtoolLink is a netdev's link as ETHTOOL_MSG_LINKMODES_GET and
// ETHTOOL_MSG_LINKSTATE_GET report it.
type EthtoolLink struct {
	// HasModes is set when the link modes were read.
	HasModes bool
	// SpeedMbps is the current speed, 0 when unknown.
	SpeedMbps int
	// Duplex is "full", "half" or "unknown".
	Duplex string
	// Autoneg is "on" or "off", "" when unknown.
	Autoneg string
	// SupportedMaxMbps and AdvertisedMaxMbps are the fastest supported and
	// advertised link modes, 0 when no mode names a speed.
	SupportedMaxMbps  int
	AdvertisedMaxMbps int

	// HasLink is set when the driver reported the link state.
	HasLink bool
	Link    bool
}

// include/uapi/linux/ethtool.h
const (
	ethtoolDuplexHalf    = 0x00
	ethtoolDuplexFull    = 0x01
	ethtoolAutonegEnable = 0x01
	ethtoolSpeedUnknown  = 0xffffffff
)

// ethtoolTimeout bounds the reads of one netdev.
const ethtoolTimeout = 2 * time.Second

// linkModeSpeedRegex takes the speed in Mb/s from a link mode name such as
// "25000baseCR/Full".
var linkModeSpeedRegex = regexp.MustCompile(`^(\d+)base`)

// NewEthtool returns the real ethtool netlink implementation.
func NewEthtool() EthtoolOps { return realEthtool{} }

type realEthtool struct{}

func (realEthtool) GetLink(ifName string) (EthtoolLink, error) {
	conn, err := genetlink.Dial(&mdnetlink.Config{Strict: true})
	if err != nil {
		return EthtoolLink{}, fmt.Errorf("dial generic netlink: %w", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(ethtoolTimeout)); err != nil {
		return EthtoolLink{}, fmt.Errorf("generic netlink deadline: %w", err)
	}
	family, err := conn.GetFamily(unix.ETHTOOL_GENL_NAME)
	if err != nil {
		return EthtoolLink{}, fmt.Errorf("generic netlink family %q: %w", unix.ETHTOOL_GENL_NAME, err)
	}

	var link EthtoolLink
	var errs []error
	if data, err := ethtoolGet(conn, family.ID, unix.ETHTOOL_MSG_LINKMODES_GET, unix.ETHTOOL_A_LINKMODES_HEADER, ifName); err != nil {
		errs = append(errs, fmt.Errorf("link modes of %s: %w", ifName, err))
	} else if err := parseLinkModes(data, &link); err != nil {
		errs = append(errs, fmt.Errorf("link modes of %s: %w", ifName, err))
	}
	if data, err := ethtoolGet(conn, family.ID, unix.ETHTOOL_MSG_LINKSTATE_GET, unix.ETHTOOL_A_LINKSTATE_HEADER, ifName); err != nil {
		errs = append(errs, fmt.Errorf("link state of %s: %w", ifName, err))
	} else if err := parseLinkState(data, &link); err != nil {
		errs = append(errs, fmt.Errorf("link state of %s: %w", ifName, err))
	}
	return link, errors.Join(errs...)
}

// ethtoolGet sends one GET request for ifName and returns the attributes of
// the reply. Without ETHTOOL_FLAG_COMPACT_BITSETS bitsets come in verbose
// form, with bit names.
func ethtoolGet(conn *genetlink.Conn, family uint16, cmd uint8, header uint16, ifName string) ([]byte, error) {
	ae := mdnetlink.NewAttributeEncoder()
	ae.Nested(header, func(nae *mdnetlink.AttributeEncoder) error {
		nae.String(unix.ETHTOOL_A_HEADER_DEV_NAME, ifName)
		return nil
	})
	data, err := ae.Encode()
	if err != nil {
		return nil, err
	}
	msgs, err := conn.Execute(genetlink.Message{
		Header: genetlink.Header{Command: cmd, Version: unix.ETHTOOL_GENL_VERSION},
		Data:   data,
	}, family, mdnetlink.Request)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, errors.New("empty reply")
	}
	return msgs[0].Data, nil
}

// parseLinkModes decodes an ETHTOOL_MSG_LINKMODES_GET reply. The OURS
// bitset's mask is the supported modes and its value the advertised ones.
func parseLinkModes(data []byte, link *EthtoolLink) error {
	ad, err := mdnetlink.NewAttributeDecoder(data)
	if err != nil {
		return err
	}
	link.Duplex = "unknown"
	for ad.Next() {
		switch ad.Type() {
		case unix.ETHTOOL_A_LINKMODES_AUTONEG:
			link.Autoneg = "off"
			if ad.Uint8() == ethtoolAutonegEnable {
				link.Autoneg = "on"
			}
		case unix.ETHTOOL_A_LINKMODES_SPEED:
			if speed := ad.Uint32(); speed != 0 && speed != ethtoolSpeedUnknown {
				link.SpeedMbps = int(speed)
			}
		case unix.ETHTOOL_A_LINKMODES_DUPLEX:
			switch ad.Uint8() {
			case ethtoolDuplexHalf:
				link.Duplex = "half"
			case ethtoolDuplexFull:
				link.Duplex = "full"
			}
		case unix.ETHTOOL_A_LINKMODES_OURS:
			ad.Nested(func(nad *mdnetlink.AttributeDecoder) error {
				supported, advertised, err := parseVerboseBitset(nad)
				link.SupportedMaxMbps = maxLinkModeSpeed(supported)
				link.AdvertisedMaxMbps = maxLinkModeSpeed(advertised)
				return err
			})
		}
	}
	if err := ad.Err(); err != nil {
		return err
	}
	link.HasModes = true
	return nil
}

// parseLinkState decodes an ETHTOOL_MSG_LINKSTATE_GET reply; the link
// attribute is absent when the driver cannot tell.
func parseLinkState(data []byte, link *EthtoolLink) error {
	ad, err := mdnetlink.NewAttributeDecoder(data)
	if err != nil {
		return err
	}
	for ad.Next() {
		if ad.Type() == unix.ETHTOOL_A_LINKSTATE_LINK {
			link.HasLink = true
			link.Link = ad.Uint8() != 0
		}
	}
	return ad.Err()
}

// parseVerboseBitset returns the bit names of a verbose-form bitset: the
// listed bits are the mask, those carrying a value flag the value. A bitset
// without a mask lists only its value bits; a compact one carries no names.
func parseVerboseBitset(ad *mdnetlink.AttributeDecoder) (mask, value []string, err error) {
	noMask := false
	var listed []string
	for ad.Next() {
		switch ad.Type() {
		case unix.ETHTOOL_A_BITSET_NOMASK:
			noMask = true
		case unix.ETHTOOL_A_BITSET_BITS:
			ad.Nested(func(bits *mdnetlink.AttributeDecoder) error {
				for bits.Next() {
					if bits.Type() != unix.ETHTOOL_A_BITSET_BITS_BIT {
						continue
					}
					var name string
					var set bool
					bits.Nested(func(bit *mdnetlink.AttributeDecoder) error {
						for bit.Next() {
							switch bit.Type() {
							case unix.ETHTOOL_A_BITSET_BIT_NAME:
								name = bit.String()
							case unix.ETHTOOL_A_BITSET_BIT_VALUE:
								set = true
							}
						}
						return bit.Err()
					})
					listed = append(listed, name)
					if set {
						value = append(value, name)
					}
				}
				return bits.Err()
			})
		}
	}
	if err := ad.Err(); err != nil {
		return nil, nil, err
	}
	if noMask {
		return nil, listed, nil
	}
	return listed, value, nil
}

// maxLinkModeSpeed returns the highest speed in Mb/s the link mode names
// carry, 0 when none does.
func maxLinkModeSpeed(modes []string) int {
	best := 0
	for _, mode := range modes {
		m := linkModeSpeedRegex.FindStringSubmatch(mode)
		if m == nil {
			continue
		}
		if speed, err := strconv.Atoi(m[1]); err == nil && speed > best {
			best = speed
		}
	}
	return best
}
