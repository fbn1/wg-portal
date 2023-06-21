package wireguard

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/h44z/wg-portal/internal/app"
	"time"

	"github.com/h44z/wg-portal/internal"
	"github.com/sirupsen/logrus"

	evbus "github.com/vardius/message-bus"

	"github.com/h44z/wg-portal/internal/config"
	"github.com/h44z/wg-portal/internal/domain"
)

type Manager struct {
	cfg *config.Config
	bus evbus.MessageBus

	db InterfaceAndPeerDatabaseRepo
	wg InterfaceController
}

func NewWireGuardManager(cfg *config.Config, bus evbus.MessageBus, wg InterfaceController, db InterfaceAndPeerDatabaseRepo) (*Manager, error) {
	m := &Manager{
		cfg: cfg,
		bus: bus,
		wg:  wg,
		db:  db,
	}

	m.connectToMessageBus()

	return m, nil
}

func (m Manager) connectToMessageBus() {
	_ = m.bus.Subscribe(app.TopicUserCreated, m.handleUserCreationEvent)
}

func (m Manager) handleUserCreationEvent(user *domain.User) {
	logrus.Errorf("Handling new user event for %s", user.Identifier)

	err := m.CreateDefaultPeer(context.Background(), user)
	if err != nil {
		logrus.Errorf("Failed to create default peer")
		return
	}
}

func (m Manager) GetImportableInterfaces(ctx context.Context) ([]domain.PhysicalInterface, error) {
	physicalInterfaces, err := m.wg.GetInterfaces(ctx)
	if err != nil {
		return nil, err
	}

	return physicalInterfaces, nil
}

func (m Manager) ImportNewInterfaces(ctx context.Context, filter ...domain.InterfaceIdentifier) error {
	physicalInterfaces, err := m.wg.GetInterfaces(ctx)
	if err != nil {
		return err
	}

	// if no filter is given, exclude already existing interfaces
	var excludedInterfaces []domain.InterfaceIdentifier
	if len(filter) == 0 {
		existingInterfaces, err := m.db.GetAllInterfaces(ctx)
		if err != nil {
			return err
		}
		for _, existingInterface := range existingInterfaces {
			excludedInterfaces = append(excludedInterfaces, existingInterface.Identifier)
		}
	}

	for _, physicalInterface := range physicalInterfaces {
		if internal.SliceContains(excludedInterfaces, physicalInterface.Identifier) {
			continue
		}

		if len(filter) != 0 && !internal.SliceContains(filter, physicalInterface.Identifier) {
			continue
		}

		logrus.Infof("importing new interface %s...", physicalInterface.Identifier)

		physicalPeers, err := m.wg.GetPeers(ctx, physicalInterface.Identifier)
		if err != nil {
			return err
		}

		err = m.importInterface(ctx, &physicalInterface, physicalPeers)
		if err != nil {
			return fmt.Errorf("import of %s failed: %w", physicalInterface.Identifier, err)
		}

		logrus.Infof("imported new interface %s and %d peers", physicalInterface.Identifier, len(physicalPeers))
	}

	return nil
}

func (m Manager) importInterface(ctx context.Context, in *domain.PhysicalInterface, peers []domain.PhysicalPeer) error {
	now := time.Now()
	iface := domain.ConvertPhysicalInterface(in)
	iface.BaseModel = domain.BaseModel{
		CreatedBy: "importer",
		UpdatedBy: "importer",
		CreatedAt: now,
		UpdatedAt: now,
	}
	iface.PeerDefAllowedIPsStr = iface.AddressStr()

	existingInterface, err := m.db.GetInterface(ctx, iface.Identifier)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if existingInterface != nil {
		return errors.New("interface already exists")
	}

	err = m.db.SaveInterface(ctx, iface.Identifier, func(_ *domain.Interface) (*domain.Interface, error) {
		return iface, nil
	})
	if err != nil {
		return fmt.Errorf("database save failed: %w", err)
	}

	// import peers
	for _, peer := range peers {
		err = m.importPeer(ctx, iface, &peer)
		if err != nil {
			return fmt.Errorf("import of peer %s failed: %w", peer.Identifier, err)
		}
	}

	return nil
}

func (m Manager) importPeer(ctx context.Context, in *domain.Interface, p *domain.PhysicalPeer) error {
	now := time.Now()
	peer := domain.ConvertPhysicalPeer(p)
	peer.BaseModel = domain.BaseModel{
		CreatedBy: "importer",
		UpdatedBy: "importer",
		CreatedAt: now,
		UpdatedAt: now,
	}

	peer.InterfaceIdentifier = in.Identifier
	peer.EndpointPublicKey = in.PublicKey
	peer.AllowedIPsStr = domain.StringConfigOption{Value: in.PeerDefAllowedIPsStr, Overridable: true}
	peer.Interface.Addresses = p.AllowedIPs // use allowed IP's as the peer IP's
	peer.Interface.DnsStr = domain.StringConfigOption{Value: in.PeerDefDnsStr, Overridable: true}
	peer.Interface.DnsSearchStr = domain.StringConfigOption{Value: in.PeerDefDnsSearchStr, Overridable: true}
	peer.Interface.Mtu = domain.IntConfigOption{Value: in.PeerDefMtu, Overridable: true}
	peer.Interface.FirewallMark = domain.Int32ConfigOption{Value: in.PeerDefFirewallMark, Overridable: true}
	peer.Interface.RoutingTable = domain.StringConfigOption{Value: in.PeerDefRoutingTable, Overridable: true}
	peer.Interface.PreUp = domain.StringConfigOption{Value: in.PeerDefPreUp, Overridable: true}
	peer.Interface.PostUp = domain.StringConfigOption{Value: in.PeerDefPostUp, Overridable: true}
	peer.Interface.PreDown = domain.StringConfigOption{Value: in.PeerDefPreDown, Overridable: true}
	peer.Interface.PostDown = domain.StringConfigOption{Value: in.PeerDefPostDown, Overridable: true}

	switch in.Type {
	case domain.InterfaceTypeAny:
		peer.Interface.Type = domain.InterfaceTypeAny
		peer.DisplayName = "Autodetected Peer (" + peer.Interface.PublicKey[0:8] + ")"
	case domain.InterfaceTypeClient:
		peer.Interface.Type = domain.InterfaceTypeServer
		peer.DisplayName = "Autodetected Endpoint (" + peer.Interface.PublicKey[0:8] + ")"
	case domain.InterfaceTypeServer:
		peer.Interface.Type = domain.InterfaceTypeClient
		peer.DisplayName = "Autodetected Client (" + peer.Interface.PublicKey[0:8] + ")"
	}

	err := m.db.SavePeer(ctx, peer.Identifier, func(_ *domain.Peer) (*domain.Peer, error) {
		return peer, nil
	})
	if err != nil {
		return fmt.Errorf("database save failed: %w", err)
	}

	return nil
}

func (m Manager) RestoreInterfaceState(ctx context.Context, updateDbOnError bool, filter ...domain.InterfaceIdentifier) error {
	interfaces, err := m.db.GetAllInterfaces(ctx)
	if err != nil {
		return err
	}

	for _, iface := range interfaces {
		if len(filter) != 0 && !internal.SliceContains(filter, iface.Identifier) {
			continue
		}

		peers, err := m.db.GetInterfacePeers(ctx, iface.Identifier)
		if err != nil {
			return fmt.Errorf("failed to load peers for %s: %w", iface.Identifier, err)
		}

		physicalInterface, err := m.wg.GetInterface(ctx, iface.Identifier)
		if err != nil {
			// try to create a new interface
			err := m.wg.SaveInterface(ctx, iface.Identifier, func(pi *domain.PhysicalInterface) (*domain.PhysicalInterface, error) {
				domain.MergeToPhysicalInterface(pi, &iface)

				return pi, nil
			})
			if err != nil {
				if updateDbOnError {
					// disable interface in database as no physical interface exists
					_ = m.db.SaveInterface(ctx, iface.Identifier, func(in *domain.Interface) (*domain.Interface, error) {
						now := time.Now()
						in.Disabled = &now // set
						in.DisabledReason = "no physical interface available"
						return in, nil
					})
				}
				return fmt.Errorf("failed to create physical interface %s: %w", iface.Identifier, err)
			}

			// restore peers
			for _, peer := range peers {
				err := m.wg.SavePeer(ctx, iface.Identifier, peer.Identifier, func(pp *domain.PhysicalPeer) (*domain.PhysicalPeer, error) {
					domain.MergeToPhysicalPeer(pp, &peer)
					return pp, nil
				})
				if err != nil {
					return fmt.Errorf("failed to create physical peer %s: %w", peer.Identifier, err)
				}
			}
		} else {
			if physicalInterface.DeviceUp != !iface.IsDisabled() {
				// try to move interface to stored state
				err := m.wg.SaveInterface(ctx, iface.Identifier, func(pi *domain.PhysicalInterface) (*domain.PhysicalInterface, error) {
					pi.DeviceUp = !iface.IsDisabled()

					return pi, nil
				})
				if err != nil {
					if updateDbOnError {
						// disable interface in database as no physical interface is available
						_ = m.db.SaveInterface(ctx, iface.Identifier, func(in *domain.Interface) (*domain.Interface, error) {
							if iface.IsDisabled() {
								now := time.Now()
								in.Disabled = &now // set
								in.DisabledReason = "no physical interface active"
							} else {
								in.Disabled = nil
								in.DisabledReason = ""
							}
							return in, nil
						})
					}
					return fmt.Errorf("failed to change physical interface state for %s: %w", iface.Identifier, err)
				}
			}
		}
	}

	return nil
}

func (m Manager) CreateDefaultPeer(ctx context.Context, user *domain.User) error {
	// TODO: implement
	return nil
}

func (m Manager) GetInterfaceAndPeers(ctx context.Context, id domain.InterfaceIdentifier) (*domain.Interface, []domain.Peer, error) {
	return m.db.GetInterfaceAndPeers(ctx, id)
}

func (m Manager) GetAllInterfaces(ctx context.Context) ([]domain.Interface, error) {
	return m.db.GetAllInterfaces(ctx)
}

func (m Manager) GetUserPeers(ctx context.Context, id domain.UserIdentifier) ([]domain.Peer, error) {
	return m.db.GetUserPeers(ctx, id)
}

func (m Manager) PrepareInterface(ctx context.Context) (*domain.Interface, error) {
	currentUser := domain.GetUserInfo(ctx)

	kp, err := domain.NewFreshKeypair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate keys: %w", err)
	}

	id, err := m.getNewInterfaceName(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to generate new identifier: %w", err)
	}

	ipv4, ipv6, err := m.getFreshIpConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to generate new ip config: %w", err)
	}

	port, err := m.getFreshListenPort(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to generate new listen port: %w", err)
	}

	ips := []domain.Cidr{ipv4}
	if m.cfg.Advanced.UseIpV6 {
		ips = append(ips, ipv6)
	}
	networks := []domain.Cidr{ipv4.NetworkAddr()}
	if m.cfg.Advanced.UseIpV6 {
		networks = append(networks, ipv6.NetworkAddr())
	}

	freshInterface := &domain.Interface{
		BaseModel: domain.BaseModel{
			CreatedBy: string(currentUser.Id),
			UpdatedBy: string(currentUser.Id),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		Identifier:                 id,
		KeyPair:                    kp,
		ListenPort:                 port,
		Addresses:                  ips,
		DnsStr:                     "",
		DnsSearchStr:               "",
		Mtu:                        1420,
		FirewallMark:               0,
		RoutingTable:               "",
		PreUp:                      "",
		PostUp:                     "",
		PreDown:                    "",
		PostDown:                   "",
		SaveConfig:                 m.cfg.Advanced.ConfigStoragePath != "",
		DisplayName:                string(id),
		Type:                       domain.InterfaceTypeServer,
		DriverType:                 "",
		Disabled:                   nil,
		DisabledReason:             "",
		PeerDefNetworkStr:          domain.CidrsToString(networks),
		PeerDefDnsStr:              "",
		PeerDefDnsSearchStr:        "",
		PeerDefEndpoint:            "",
		PeerDefAllowedIPsStr:       domain.CidrsToString(networks),
		PeerDefMtu:                 1420,
		PeerDefPersistentKeepalive: 16,
		PeerDefFirewallMark:        0,
		PeerDefRoutingTable:        "",
		PeerDefPreUp:               "",
		PeerDefPostUp:              "",
		PeerDefPreDown:             "",
		PeerDefPostDown:            "",
	}

	return freshInterface, nil
}

func (m Manager) getNewInterfaceName(ctx context.Context) (domain.InterfaceIdentifier, error) {
	namePrefix := "wg"
	nameSuffix := 0

	existingInterfaces, err := m.db.GetAllInterfaces(ctx)
	if err != nil {
		return "", err
	}
	var name domain.InterfaceIdentifier
	for {
		name = domain.InterfaceIdentifier(fmt.Sprintf("%s%d", namePrefix, nameSuffix))

		conflict := false
		for _, in := range existingInterfaces {
			if in.Identifier == name {
				conflict = true
				break
			}
		}
		if !conflict {
			break
		}

		nameSuffix++
	}

	return name, nil
}

func (m Manager) getFreshIpConfig(ctx context.Context) (ipV4, ipV6 domain.Cidr, err error) {
	ips, err := m.db.GetInterfaceIps(ctx)
	if err != nil {
		err = fmt.Errorf("failed to get existing IP addresses: %w", err)
		return
	}

	useV6 := m.cfg.Advanced.UseIpV6
	ipV4, _ = domain.CidrFromString(m.cfg.Advanced.StartCidrV4)
	ipV6, _ = domain.CidrFromString(m.cfg.Advanced.StartCidrV6)

	for {
		ipV4Conflict := false
		ipV6Conflict := false
		for _, usedIps := range ips {
			for _, ip := range usedIps {
				if ipV4 == ip {
					ipV4Conflict = true
				}

				if ipV6 == ip {
					ipV6Conflict = true
				}
			}
		}

		if !ipV4Conflict && (!useV6 || !ipV6Conflict) {
			break
		}

		if ipV4Conflict {
			ipV4 = ipV4.NextSubnet()
		}

		if ipV6Conflict && useV6 {
			ipV6 = ipV6.NextSubnet()
		}
	}

	return
}

func (m Manager) getFreshListenPort(ctx context.Context) (port int, err error) {
	existingInterfaces, err := m.db.GetAllInterfaces(ctx)
	if err != nil {
		return -1, err
	}

	port = m.cfg.Advanced.StartListenPort

	for {
		conflict := false
		for _, in := range existingInterfaces {
			if in.ListenPort == port {
				conflict = true
				break
			}
		}
		if !conflict {
			break
		}

		port++
	}

	if port > 65535 { // maximum allowed port number (16 bit uint)
		return -1, fmt.Errorf("port space exhausted")
	}

	return
}

func (m Manager) CreateInterface(ctx context.Context, in *domain.Interface) (*domain.Interface, error) {
	existingInterface, err := m.db.GetInterface(ctx, in.Identifier)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, fmt.Errorf("unable to load existing interface %s: %w", in.Identifier, err)
	}
	if existingInterface != nil {
		return nil, fmt.Errorf("interface %s already exists", in.Identifier)
	}

	if err := m.validateCreation(ctx, existingInterface, in); err != nil {
		return nil, fmt.Errorf("creation not allowed: %w", err)
	}

	err = m.db.SaveInterface(ctx, in.Identifier, func(i *domain.Interface) (*domain.Interface, error) {
		in.CopyCalculatedAttributes(i)

		err = m.wg.SaveInterface(ctx, in.Identifier, func(pi *domain.PhysicalInterface) (*domain.PhysicalInterface, error) {
			domain.MergeToPhysicalInterface(pi, in)
			return pi, nil
		})
		if err != nil {
			return nil, fmt.Errorf("failed to create physical interface %s: %w", in.Identifier, err)
		}

		return in, nil
	})
	if err != nil {
		return nil, fmt.Errorf("creation failure: %w", err)
	}

	return in, nil
}

func (m Manager) UpdateInterface(ctx context.Context, in *domain.Interface) (*domain.Interface, error) {
	existingInterface, err := m.db.GetInterface(ctx, in.Identifier)
	if err != nil {
		return nil, fmt.Errorf("unable to load existing interface %s: %w", in.Identifier, err)
	}

	if err := m.validateModifications(ctx, existingInterface, in); err != nil {
		return nil, fmt.Errorf("update not allowed: %w", err)
	}

	err = m.db.SaveInterface(ctx, in.Identifier, func(i *domain.Interface) (*domain.Interface, error) {
		in.CopyCalculatedAttributes(i)

		err = m.wg.SaveInterface(ctx, in.Identifier, func(pi *domain.PhysicalInterface) (*domain.PhysicalInterface, error) {
			domain.MergeToPhysicalInterface(pi, in)
			return pi, nil
		})
		if err != nil {
			return nil, fmt.Errorf("failed to update physical interface %s: %w", in.Identifier, err)
		}

		return in, nil
	})
	if err != nil {
		return nil, fmt.Errorf("update failure: %w", err)
	}

	return in, nil
}

func (m Manager) DeleteInterface(ctx context.Context, id domain.InterfaceIdentifier) error {
	existingInterface, err := m.db.GetInterface(ctx, id)
	if err != nil {
		return fmt.Errorf("unable to find interface %s: %w", id, err)
	}

	if err := m.validateDeletion(ctx, existingInterface); err != nil {
		return fmt.Errorf("deletion not allowed: %w", err)
	}

	err = m.deleteInterfacePeers(ctx, id)
	if err != nil {
		return fmt.Errorf("peer deletion failure: %w", err)
	}

	err = m.wg.DeleteInterface(ctx, id)
	if err != nil {
		return fmt.Errorf("wireguard deletion failure: %w", err)
	}

	err = m.db.DeleteInterface(ctx, id)
	if err != nil {
		return fmt.Errorf("deletion failure: %w", err)
	}

	return nil
}

func (m Manager) validateModifications(ctx context.Context, old, new *domain.Interface) error {
	currentUser := domain.GetUserInfo(ctx)

	if !currentUser.IsAdmin {
		return fmt.Errorf("insufficient permissions")
	}

	return nil
}

func (m Manager) validateCreation(ctx context.Context, old, new *domain.Interface) error {
	currentUser := domain.GetUserInfo(ctx)

	if new.Identifier == "" {
		return fmt.Errorf("invalid interface identifier")
	}

	if !currentUser.IsAdmin {
		return fmt.Errorf("insufficient permissions")
	}

	return nil
}

func (m Manager) validateDeletion(ctx context.Context, del *domain.Interface) error {
	currentUser := domain.GetUserInfo(ctx)

	if !currentUser.IsAdmin {
		return fmt.Errorf("insufficient permissions")
	}

	return nil
}

func (m Manager) deleteInterfacePeers(ctx context.Context, id domain.InterfaceIdentifier) error {
	allPeers, err := m.db.GetInterfacePeers(ctx, id)
	if err != nil {
		return err
	}
	for _, peer := range allPeers {
		err = m.wg.DeletePeer(ctx, id, peer.Identifier)
		if err != nil {
			return fmt.Errorf("wireguard peer deletion failure for %s: %w", peer.Identifier, err)
		}

		err = m.db.DeletePeer(ctx, peer.Identifier)
		if err != nil {
			return fmt.Errorf("peer deletion failure for %s: %w", peer.Identifier, err)
		}
	}

	return nil
}

func (m Manager) PreparePeer(ctx context.Context, id domain.InterfaceIdentifier) (*domain.Peer, error) {
	currentUser := domain.GetUserInfo(ctx)

	iface, err := m.db.GetInterface(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("unable to find interface %s: %w", id, err)
	}

	kp, err := domain.NewFreshKeypair()
	if err != nil {
		return nil, fmt.Errorf("failed to generate keys: %w", err)
	}

	pk, err := domain.NewPreSharedKey()
	if err != nil {
		return nil, fmt.Errorf("failed to generate preshared key: %w", err)
	}

	peerMode := domain.InterfaceTypeClient
	if iface.Type == domain.InterfaceTypeClient {
		peerMode = domain.InterfaceTypeServer
	}

	freshPeer := &domain.Peer{
		BaseModel: domain.BaseModel{
			CreatedBy: string(currentUser.Id),
			UpdatedBy: string(currentUser.Id),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		},
		Endpoint:            domain.StringConfigOption{},
		EndpointPublicKey:   "",
		AllowedIPsStr:       domain.StringConfigOption{},
		ExtraAllowedIPsStr:  "",
		PresharedKey:        pk,
		PersistentKeepalive: domain.IntConfigOption{},
		DisplayName:         "",
		Identifier:          domain.PeerIdentifier(uuid.New().String()),
		UserIdentifier:      currentUser.Id,
		InterfaceIdentifier: iface.Identifier,
		Disabled:            nil,
		DisabledReason:      "",
		ExpiresAt:           nil,
		Notes:               "",
		Interface: domain.PeerInterfaceConfig{
			KeyPair:           kp,
			Type:              peerMode,
			Addresses:         nil,
			CheckAliveAddress: "",
			DnsStr:            domain.StringConfigOption{},
			DnsSearchStr:      domain.StringConfigOption{},
			Mtu:               domain.IntConfigOption{},
			FirewallMark:      domain.Int32ConfigOption{},
			RoutingTable:      domain.StringConfigOption{},
			PreUp:             domain.StringConfigOption{},
			PostUp:            domain.StringConfigOption{},
			PreDown:           domain.StringConfigOption{},
			PostDown:          domain.StringConfigOption{},
		},
	}

	return freshPeer, nil
}

func (m Manager) DeletePeer(ctx context.Context, id domain.PeerIdentifier) error {
	peer, err := m.db.GetPeer(ctx, id)
	if err != nil {
		return fmt.Errorf("unable to find peer %s: %w", id, err)
	}

	err = m.wg.DeletePeer(ctx, peer.InterfaceIdentifier, id)
	if err != nil {
		return fmt.Errorf("wireguard failed to delete peer %s: %w", id, err)
	}

	err = m.db.DeletePeer(ctx, id)
	if err != nil {
		return fmt.Errorf("failed to delete peer %s: %w", id, err)
	}

	return nil
}

func (m Manager) GetPeer(ctx context.Context, id domain.PeerIdentifier) (*domain.Peer, error) {
	peer, err := m.db.GetPeer(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("unable to find peer %s: %w", id, err)
	}

	return peer, nil
}
