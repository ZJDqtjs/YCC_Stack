package store

import "database/sql"

// VMOfferRow and VMInstanceRow are storage shapes, not domain objects: ports and
// endpoints stay JSON so the SQLite schema does not have to grow a table for
// every mapping, and the hub is the only place that knows their meaning.
type VMOfferRow struct {
	ID, NodeID, OwnerID, Name          string
	ImageURL, ImageSHA256, ImageFormat string
	OS                                 string
	CPUs, MemMB, DiskGB                int
	NetMode, IPv6Prefix, IPv6Gateway   string
	HostIPv6                           string
	Ports                              string
	PriceHour                          float64
	MaxHours                           int
	Enabled                            bool
	CreatedAt                          int64
}

type VMInstanceRow struct {
	ID, OfferID, NodeID, BuyerID, SellerID string
	Name, Status                           string
	CPUs, MemMB, DiskGB                    int
	ImageURL, ImageSHA256, ImageFormat     string
	OS, NetMode                            string
	GuestIPv6, HostIPv6, Gateway           string
	PrefixLen                              int
	Ports                                  string
	SSHUser, SSHPublicKey                  string
	SSHPort, ConsolePort                   int
	RootPasswordEnc, VNCPasswordEnc        string
	PriceHour, Hold, Spent                 float64
	MaxHours                               int
	Progress                               float64
	Endpoints, Note, Err                   string
	Cleanup                                bool
	CreatedAt, StartedAt, StoppedAt        int64
	LastSeen, ExpiresAt, StoppingAt        int64
}

func (s *Store) InsertVMOffer(o *VMOfferRow) error {
	_, err := s.db.Exec(`INSERT INTO vm_offers(id,node_id,owner_id,name,image_url,image_sha256,
			image_format,os,cpus,mem_mb,disk_gb,net_mode,ipv6_prefix,ipv6_gateway,host_ipv6,ports,
			price_hour,max_hours,enabled,created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.NodeID, o.OwnerID, o.Name, o.ImageURL, o.ImageSHA256, o.ImageFormat, o.OS,
		o.CPUs, o.MemMB, o.DiskGB, o.NetMode, o.IPv6Prefix, o.IPv6Gateway, o.HostIPv6, o.Ports,
		o.PriceHour, o.MaxHours, b2i(o.Enabled), o.CreatedAt)
	return err
}

func (s *Store) SetVMOfferEnabled(id, ownerID string, enabled bool) (bool, error) {
	res, err := s.db.Exec(`UPDATE vm_offers SET enabled=? WHERE id=? AND owner_id=?`, b2i(enabled), id, ownerID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *Store) DeleteVMOffer(id, ownerID string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM vm_offers WHERE id=? AND owner_id=?`, id, ownerID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

const vmOfferCols = `id,node_id,owner_id,name,image_url,image_sha256,image_format,os,cpus,mem_mb,
	disk_gb,net_mode,ipv6_prefix,ipv6_gateway,host_ipv6,ports,price_hour,max_hours,enabled,created_at`

func (s *Store) VMOffers() ([]*VMOfferRow, error) {
	rows, err := s.db.Query(`SELECT ` + vmOfferCols + ` FROM vm_offers ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*VMOfferRow
	for rows.Next() {
		o := &VMOfferRow{}
		var enabled int
		if err := rows.Scan(&o.ID, &o.NodeID, &o.OwnerID, &o.Name, &o.ImageURL, &o.ImageSHA256,
			&o.ImageFormat, &o.OS, &o.CPUs, &o.MemMB, &o.DiskGB, &o.NetMode, &o.IPv6Prefix,
			&o.IPv6Gateway, &o.HostIPv6, &o.Ports, &o.PriceHour, &o.MaxHours, &enabled, &o.CreatedAt); err != nil {
			return nil, err
		}
		o.Enabled = enabled != 0
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) VMOffer(id string) (*VMOfferRow, error) {
	rows, err := s.db.Query(`SELECT `+vmOfferCols+` FROM vm_offers WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	o := &VMOfferRow{}
	var enabled int
	if err := rows.Scan(&o.ID, &o.NodeID, &o.OwnerID, &o.Name, &o.ImageURL, &o.ImageSHA256,
		&o.ImageFormat, &o.OS, &o.CPUs, &o.MemMB, &o.DiskGB, &o.NetMode, &o.IPv6Prefix,
		&o.IPv6Gateway, &o.HostIPv6, &o.Ports, &o.PriceHour, &o.MaxHours, &enabled, &o.CreatedAt); err != nil {
		return nil, err
	}
	o.Enabled = enabled != 0
	return o, rows.Err()
}

func (s *Store) InsertVMInstance(v *VMInstanceRow) error {
	_, err := s.db.Exec(`INSERT INTO vm_instances(id,offer_id,node_id,buyer_id,seller_id,name,status,
			cpus,mem_mb,disk_gb,image_url,image_sha256,image_format,os,net_mode,guest_ipv6,
			prefix_len,gateway,host_ipv6,
			ports,ssh_user,ssh_port,console_port,ssh_public_key,root_password_enc,vnc_password_enc,
			price_hour,hold,spent,max_hours,progress,endpoints,note,err,cleanup,created_at,started_at,
			stopped_at,last_seen,expires_at,stopping_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		v.ID, v.OfferID, v.NodeID, nullStr(v.BuyerID), nullStr(v.SellerID), v.Name, v.Status,
		v.CPUs, v.MemMB, v.DiskGB, v.ImageURL, v.ImageSHA256, v.ImageFormat, v.OS, v.NetMode,
		v.GuestIPv6, v.PrefixLen, v.Gateway, v.HostIPv6, v.Ports, v.SSHUser, v.SSHPort,
		v.ConsolePort, v.SSHPublicKey, v.RootPasswordEnc, v.VNCPasswordEnc, v.PriceHour, v.Hold,
		v.Spent, v.MaxHours, v.Progress, v.Endpoints, v.Note, v.Err, b2i(v.Cleanup), v.CreatedAt,
		v.StartedAt, v.StoppedAt, v.LastSeen, v.ExpiresAt, v.StoppingAt)
	return err
}

func (s *Store) UpdateVMInstance(v *VMInstanceRow) error {
	_, err := s.db.Exec(`UPDATE vm_instances SET status=?,guest_ipv6=?,prefix_len=?,gateway=?,
			ports=?,ssh_public_key=?,root_password_enc=?,vnc_password_enc=?,hold=?,spent=?,
			progress=?,endpoints=?,note=?,err=?,
			cleanup=?,started_at=?,stopped_at=?,last_seen=?,expires_at=?,stopping_at=? WHERE id=?`,
		v.Status, v.GuestIPv6, v.PrefixLen, v.Gateway, v.Ports, v.SSHPublicKey,
		v.RootPasswordEnc, v.VNCPasswordEnc, v.Hold, v.Spent, v.Progress, v.Endpoints,
		v.Note, v.Err, b2i(v.Cleanup), v.StartedAt, v.StoppedAt, v.LastSeen, v.ExpiresAt,
		v.StoppingAt, v.ID)
	return err
}

const vmInstanceCols = `id,offer_id,node_id,buyer_id,seller_id,name,status,cpus,mem_mb,disk_gb,
	image_url,image_sha256,image_format,os,net_mode,guest_ipv6,prefix_len,gateway,host_ipv6,ports,ssh_user,ssh_port,
	console_port,ssh_public_key,root_password_enc,vnc_password_enc,price_hour,hold,spent,max_hours,
	progress,endpoints,note,err,cleanup,created_at,started_at,stopped_at,last_seen,expires_at,stopping_at`

func (s *Store) VMInstances() ([]*VMInstanceRow, error) {
	rows, err := s.db.Query(`SELECT ` + vmInstanceCols + ` FROM vm_instances ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*VMInstanceRow
	for rows.Next() {
		v := &VMInstanceRow{}
		var buyer, seller sql.NullString
		var cleanup int
		if err := rows.Scan(&v.ID, &v.OfferID, &v.NodeID, &buyer, &seller, &v.Name, &v.Status,
			&v.CPUs, &v.MemMB, &v.DiskGB, &v.ImageURL, &v.ImageSHA256, &v.ImageFormat, &v.OS,
			&v.NetMode, &v.GuestIPv6, &v.PrefixLen, &v.Gateway, &v.HostIPv6, &v.Ports, &v.SSHUser, &v.SSHPort,
			&v.ConsolePort, &v.SSHPublicKey, &v.RootPasswordEnc, &v.VNCPasswordEnc, &v.PriceHour,
			&v.Hold, &v.Spent, &v.MaxHours, &v.Progress, &v.Endpoints, &v.Note, &v.Err, &cleanup,
			&v.CreatedAt, &v.StartedAt, &v.StoppedAt, &v.LastSeen, &v.ExpiresAt, &v.StoppingAt); err != nil {
			return nil, err
		}
		v.BuyerID, v.SellerID = buyer.String, seller.String
		v.Cleanup = cleanup != 0
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) VMInstance(id string) (*VMInstanceRow, error) {
	rows, err := s.db.Query(`SELECT `+vmInstanceCols+` FROM vm_instances WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	v := &VMInstanceRow{}
	var buyer, seller sql.NullString
	var cleanup int
	if err := rows.Scan(&v.ID, &v.OfferID, &v.NodeID, &buyer, &seller, &v.Name, &v.Status,
		&v.CPUs, &v.MemMB, &v.DiskGB, &v.ImageURL, &v.ImageSHA256, &v.ImageFormat, &v.OS,
		&v.NetMode, &v.GuestIPv6, &v.PrefixLen, &v.Gateway, &v.HostIPv6, &v.Ports, &v.SSHUser, &v.SSHPort,
		&v.ConsolePort, &v.SSHPublicKey, &v.RootPasswordEnc, &v.VNCPasswordEnc, &v.PriceHour,
		&v.Hold, &v.Spent, &v.MaxHours, &v.Progress, &v.Endpoints, &v.Note, &v.Err, &cleanup,
		&v.CreatedAt, &v.StartedAt, &v.StoppedAt, &v.LastSeen, &v.ExpiresAt, &v.StoppingAt); err != nil {
		return nil, err
	}
	v.BuyerID, v.SellerID = buyer.String, seller.String
	v.Cleanup = cleanup != 0
	return v, rows.Err()
}
