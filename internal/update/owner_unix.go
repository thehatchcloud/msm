package update

import "syscall"

func owner(st any) (uid, gid int) {
	if s, ok := st.(*syscall.Stat_t); ok {
		return int(s.Uid), int(s.Gid)
	}
	return -1, -1
}
