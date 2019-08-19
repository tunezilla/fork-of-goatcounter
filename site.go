// Copyright © 2019 Martin Tournoij <martin@arp242.net>
// This file is part of GoatCounter and published under the terms of the AGPLv3,
// which can be found in the LICENSE file or at gnu.org/licenses/agpl.html

package goatcounter

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/teamwork/guru"
	"github.com/teamwork/utils/jsonutil"
	"github.com/teamwork/validate"
	"zgo.at/goatcounter/cfg"
)

// Plan column values.
const (
	PlanPersonal   = "p"
	PlanBusiness   = "b"
	PlanEnterprise = "e"
)

var Plans = []string{PlanPersonal, PlanBusiness, PlanEnterprise}

var reserved = []string{
	"goatcounter", "goatcounters",
	"www", "mail", "smtp", "imap", "static",
	"admin", "ns1", "ns2", "m", "mobile", "api",
}

// Site is a single site which is sending newsletters (i.e. it's a "customer").
type Site struct {
	ID int64 `db:"id"`

	Domain       string       `db:"domain"` // Domain for which the service is (arp242.net)
	Code         string       `db:"code"`   // Domain code (arp242, which makes arp242.goatcounter.com)
	Plan         string       `db:"plan"`
	Stripe       *string      `db:"stripe"`
	Settings     SiteSettings `db:"settings"`
	LastStat     *time.Time   `db:"last_stat"`
	ReceivedData bool         `db:"received_data"`

	State     string     `db:"state"`
	CreatedAt time.Time  `db:"created_at"`
	UpdatedAt *time.Time `db:"updated_at"`
}

type SiteSettings struct {
	Public          bool   `json:"public"`
	TwentyFourHours bool   `json:"twenty_four_hours"`
	DateFormat      string `json:"date_format"`
	Limits          struct {
		Page    int `json:"page"`
		Ref     int `json:"ref"`
		Browser int `json:"browser"`
	} `json:"limits"`
}

func (ss SiteSettings) String() string { return string(jsonutil.MustMarshal(ss)) }

// Value implements the SQL Value function to determine what to store in the DB.
func (ss SiteSettings) Value() (driver.Value, error) { return json.Marshal(ss) }

// Scan converts the data returned from the DB into the struct.
func (ss *SiteSettings) Scan(v interface{}) error {
	switch vv := v.(type) {
	case []byte:
		return json.Unmarshal(vv, ss)
	case string:
		return json.Unmarshal([]byte(vv), ss)
	default:
		panic(fmt.Sprintf("unsupported type: %T", v))
	}
}

// Defaults sets fields to default values, unless they're already set.
func (s *Site) Defaults(ctx context.Context) {
	if s.State == "" {
		s.State = StateActive
	}

	if s.Settings.DateFormat == "" {
		s.Settings.DateFormat = "2006-01-02"
	}

	if s.Settings.Limits.Page == 0 {
		s.Settings.Limits.Page = 20
	}
	if s.Settings.Limits.Ref == 0 {
		s.Settings.Limits.Ref = 10
	}
	if s.Settings.Limits.Browser == 0 {
		s.Settings.Limits.Browser = 20
	}

	s.Code = strings.ToLower(s.Code)

	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	} else {
		t := time.Now().UTC()
		s.UpdatedAt = &t
	}
}

// Validate the object.
func (s *Site) Validate(ctx context.Context) error {
	v := validate.New()

	v.Required("domain", s.Domain)
	v.Required("code", s.Code)
	v.Required("state", s.State)
	v.Required("plan", s.Plan)
	v.Include("state", s.State, States)
	v.Include("plan", s.Plan, Plans)

	v.Len("code", s.Code, 1, 50)
	v.Len("domain", s.Domain, 4, 255)
	v.Domain("domain", s.Domain)
	v.Exclude("domain", s.Domain, reserved)

	if s.Stripe != nil && !strings.HasPrefix(*s.Stripe, "cus_") {
		v.Append("stripe", "not a valid Stripe customer ID")
	}

	for _, c := range s.Code {
		if !(c == 95 || (c >= 48 && c <= 57) || (c >= 97 && c <= 122)) {
			v.Append("code", fmt.Sprintf("%q not allowed; characters are limited to '_', a to z, and numbers", c))
			break
		}
	}
	if len(s.Code) > 0 && s.Code[0] == '_' { // Special domains, like _acme-challenge.
		v.Append("code", "cannot start with underscore (_)")
	}

	if !v.HasErrors() {
		var code, domain uint8
		err := MustGetDB(ctx).GetContext(ctx, &code,
			`select 1 from sites where lower(code)=lower($1) and id!=$2 limit 1`,
			s.Code, s.ID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if code == 1 {
			v.Append("code", "already exists")
		}

		err = MustGetDB(ctx).GetContext(ctx, &domain,
			`select 1 from sites where lower(domain)=lower($1) and id!=$2 limit 1`,
			s.Domain, s.ID)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if domain == 1 {
			v.Append("domain", "already exists")
		}
	}

	return v.ErrorOrNil()
}

// Insert a new row.
func (s *Site) Insert(ctx context.Context) error {
	if s.ID > 0 {
		return errors.New("ID > 0")
	}

	s.Defaults(ctx)
	err := s.Validate(ctx)
	if err != nil {
		return err
	}

	res, err := MustGetDB(ctx).ExecContext(ctx,
		`insert into sites (code, domain, settings, plan, created_at) values ($1, $2, $3, $4, $5)`,
		s.Code, s.Domain, s.Settings, s.Plan, sqlDate(s.CreatedAt))
	if err != nil {
		if uniqueErr(err) {
			return guru.New(400, "this site already exists: domain and code must be unique")
		}
		return errors.Wrap(err, "Site.Insert")
	}

	if cfg.PgSQL {
		var ns Site
		err = ns.ByCode(ctx, s.Code)
		s.ID = ns.ID
	} else {
		s.ID, err = res.LastInsertId()
	}
	return errors.Wrap(err, "Site.Insert")
}

// Update existing.
func (s *Site) Update(ctx context.Context) error {
	if s.ID == 0 {
		return errors.New("ID == 0")
	}

	s.Defaults(ctx)
	err := s.Validate(ctx)
	if err != nil {
		return err
	}

	_, err = MustGetDB(ctx).ExecContext(ctx,
		`update sites set domain=$1, settings=$2, updated_at=$3 where id=$4`,
		s.Domain, s.Settings, sqlDate(*s.UpdatedAt), s.ID)
	return errors.Wrap(err, "Site.Update")
}

// UpdateStripe sets the Stripe customer ID.
func (s *Site) UpdateStripe(ctx context.Context) error {
	if s.ID == 0 {
		return errors.New("ID == 0")
	}

	s.Defaults(ctx)
	err := s.Validate(ctx)
	if err != nil {
		return err
	}

	_, err = MustGetDB(ctx).ExecContext(ctx,
		`update sites set stripe=$1, updated_at=$2 where id=$3`,
		s.Stripe, sqlDate(*s.UpdatedAt), s.ID)
	return errors.Wrap(err, "Site.UpdateStripe")
}

// ByID gets a site by ID.
func (s *Site) ByID(ctx context.Context, id int64) error {
	return errors.Wrap(MustGetDB(ctx).GetContext(ctx, s,
		`select * from sites where id=$1 and state=$2`,
		id, StateActive), "Site.ByID")
}

// ByCode gets a site by subdomain code.
func (s *Site) ByCode(ctx context.Context, code string) error {
	return errors.Wrap(MustGetDB(ctx).GetContext(ctx, s,
		`select * from sites where lower(code)=lower($1) and state=$2`,
		code, StateActive), "Site.ByCode")
}

// Sites is a list of sites.
type Sites []Site

// List all sites.
func (u *Sites) List(ctx context.Context) error {
	return errors.Wrap(MustGetDB(ctx).SelectContext(ctx, u,
		`select * from sites order by created_at desc`),
		"Sites.List")
}
