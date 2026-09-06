package main

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
)

func (a app) team(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("team requires list, create NAME, members ID, invite ID --user ACCOUNT_ID --role reader|writer, or remove ID --user ACCOUNT_ID")
	}
	fs := newFlags("team", a.err)
	user := fs.String("user", "", "stable GitHub account ID (from moirai whoami)")
	role := fs.String("role", "reader", "reader or writer")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	c, err := loadCloud("")
	if err != nil {
		return err
	}
	method, path := "GET", "/v1/teams"
	var body any
	if args[0] == "create" {
		if fs.NArg() != 1 {
			return errors.New("team create requires a name")
		}
		method = "POST"
		body = map[string]string{"name": fs.Arg(0)}
	} else if args[0] != "list" {
		id := fs.Arg(0)
		suffix := strings.TrimPrefix(id, "team_")
		if !strings.HasPrefix(id, "team_") || len(suffix) != 48 {
			return errors.New("invalid team ID")
		}
		if _, err = hex.DecodeString(suffix); err != nil {
			return errors.New("invalid team ID")
		}
		path += "/" + id + "/members"
		switch args[0] {
		case "members":
		case "invite":
			if *user == "" {
				return errors.New("team invite requires --user ACCOUNT_ID")
			}
			method = "POST"
			body = map[string]string{"user_id": *user, "role": *role}
		case "remove":
			if *user == "" || strings.Trim(*user, "0123456789") != "" {
				return errors.New("team remove requires a numeric --user ACCOUNT_ID")
			}
			method = "DELETE"
			path += "/" + *user
		default:
			return errors.New("unknown team command")
		}
	}
	data, _, err := c.request(ctx, method, path, body, "")
	if err != nil {
		return err
	}
	if len(data) > 0 {
		_, err = a.out.Write(append(data, '\n'))
	}
	return err
}
