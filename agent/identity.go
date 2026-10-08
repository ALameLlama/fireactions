package agent

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// Identity is a resolved guest execution identity. Groups contains supplementary
// groups only; GID is the selected primary group.
type Identity struct {
	UID      uint32
	GID      uint32
	Username string
	Home     string
	Shell    string
	Groups   []uint32
}

func resolveIdentity(spec string) (Identity, error) {
	if spec == "" || strings.IndexByte(spec, 0) >= 0 {
		return Identity{}, fmt.Errorf("invalid guest user")
	}
	userSpec, groupSpec, hasGroup := strings.Cut(spec, ":")
	if userSpec == "" || (hasGroup && groupSpec == "") || strings.Contains(groupSpec, ":") {
		return Identity{}, fmt.Errorf("invalid guest user")
	}

	account, err := lookupUser(userSpec)
	if err != nil {
		return Identity{}, fmt.Errorf("guest user does not exist")
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid guest user id")
	}
	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return Identity{}, fmt.Errorf("invalid guest group id")
	}
	if hasGroup {
		group, lookupErr := lookupGroup(groupSpec)
		if lookupErr != nil {
			return Identity{}, fmt.Errorf("guest group does not exist")
		}
		gid, err = strconv.ParseUint(group.Gid, 10, 32)
		if err != nil {
			return Identity{}, fmt.Errorf("invalid guest group id")
		}
	}

	home := account.HomeDir
	if home == "" {
		home = "/"
	}
	shell := lookupShell(uint32(uid), account.Username)
	if shell == "" {
		shell = "/bin/sh"
	}

	groups := make([]uint32, 0)
	seen := map[uint32]struct{}{uint32(gid): {}}
	groupIDs, groupErr := account.GroupIds()
	if groupErr != nil {
		return Identity{}, fmt.Errorf("resolve supplementary groups")
	}
	for _, raw := range groupIDs {
		id, parseErr := strconv.ParseUint(raw, 10, 32)
		if parseErr != nil {
			return Identity{}, fmt.Errorf("invalid supplementary group id")
		}
		groupID := uint32(id)
		if _, exists := seen[groupID]; exists {
			continue
		}
		seen[groupID] = struct{}{}
		groups = append(groups, groupID)
	}

	return Identity{
		UID:      uint32(uid),
		GID:      uint32(gid),
		Username: account.Username,
		Home:     home,
		Shell:    shell,
		Groups:   groups,
	}, nil
}

func lookupUser(spec string) (*user.User, error) {
	if id, err := strconv.ParseUint(spec, 10, 32); err == nil {
		return user.LookupId(strconv.FormatUint(id, 10))
	}
	return user.Lookup(spec)
}

func lookupGroup(spec string) (*user.Group, error) {
	if id, err := strconv.ParseUint(spec, 10, 32); err == nil {
		return user.LookupGroupId(strconv.FormatUint(id, 10))
	}
	return user.LookupGroup(spec)
}

func lookupShell(uid uint32, username string) string {
	file, err := os.Open("/etc/passwd")
	if err != nil {
		return ""
	}
	defer file.Close()
	for scanner := bufio.NewScanner(file); scanner.Scan(); {
		fields := strings.Split(scanner.Text(), ":")
		if len(fields) < 7 || (fields[0] != username) {
			continue
		}
		parsedUID, parseErr := strconv.ParseUint(fields[2], 10, 32)
		if parseErr == nil && uint32(parsedUID) == uid {
			return fields[6]
		}
	}
	return ""
}
