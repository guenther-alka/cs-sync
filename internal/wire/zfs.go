package wire

// ZFS replication frames (cs-sync --mode zfs with --backup-host, receiver
// "cs-sync zfsreceive"). Added on top of the file-sync frames; the handshake
// (TWelcome / TAck) is shared and unchanged. Spec:
// csweb-gui/data/howto.ai/cs-sync-zfs-realtime.info
const (
	TZfsHello  byte = 15 // sender -> receiver: dataset id + list of primary snapshot GUIDs
	TZfsPlan   byte = 16 // receiver -> sender: what to send (full / inc / resume / rotate / blocked / error)
	TZfsChunk  byte = 17 // sender -> receiver: part of the zfs send stream
	TZfsEnd    byte = 18 // sender -> receiver: end of stream
	TZfsDone   byte = 19 // receiver -> sender: zfs recv finished (GUID of the received "new" snapshot)
	TZfsRotate byte = 20 // sender -> receiver: sender confirmed, rotate your side now
)

// ZfsSnapRef is one sync_ snapshot of the primary (slot + GUID), oldest first.
type ZfsSnapRef struct {
	Slot string
	Guid string
}

// ZfsHello opens a cycle. The primary never asks for the backup's
// snapshots; it only offers its own GUIDs and the backup picks.
type ZfsHello struct {
	Nr        string
	FsGuid    string // GUID of the primary dataset
	Snaps     []ZfsSnapRef
	Recursive bool              // replicate child datasets (zfs send -R)
	ConfOnly  bool              // only the Proxmox VM/CT config changed: no plan/stream, just apply Conf (answer: TZfsDone)
	Conf      map[string]string // relative dataset -> encoded cs:pveconf
}

// ZfsPlan is the backup's answer.
// Kind: "full" | "inc" | "resume" | "rotate" | "blocked" | "error"
type ZfsPlan struct {
	Kind  string
	Base  string // inc: GUID of the common base snapshot
	Token string // resume: receive_resume_token
	Msg   string // blocked / error text
}

type ZfsChunk struct {
	Data []byte
}

type ZfsDone struct {
	OK      bool
	Err     string
	NewGuid string // GUID of the snapshot "new" now on the backup
}

type ZfsRotate struct {
	App        bool              // snapshot was taken via cs-freeze4snap (cs:consistent=app); kept for older senders
	Consistent string            // cs:consistent value: "app" (frozen) or "paused" (VM paused, crash-consistent)
	Conf       map[string]string // relative dataset -> encoded cs:pveconf of this snapshot
}
