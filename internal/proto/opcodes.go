// Package proto implements the eD2K wire format: packet framing,
// little-endian primitives, tags and opcode constants.
package proto

// Protocol bytes (first byte of every TCP packet).
const (
	ProtoEDonkey = 0xE3
	ProtoEMule   = 0xC5
	ProtoPacked  = 0xD4 // zlib-compressed payload
	ProtoKad     = 0xE4
	ProtoKadPack = 0xE5
)

// Client -> server opcodes.
const (
	OpLoginRequest    = 0x01
	OpGetServerList   = 0x14
	OpOfferFiles      = 0x15
	OpSearchRequest   = 0x16
	OpDisconnect      = 0x18
	OpGetSources      = 0x19
	OpSearchUser      = 0x1A
	OpCallbackRequest = 0x1C
	OpQueryMoreResult = 0x21
	OpGetSourcesObfu  = 0x23
)

// Server -> client opcodes.
const (
	OpReject            = 0x05
	OpServerList        = 0x32
	OpSearchResult      = 0x33
	OpServerStatus      = 0x34
	OpCallbackRequested = 0x35
	OpCallbackFail      = 0x36
	OpServerMessage     = 0x38
	OpIDChange          = 0x40
	OpServerIdent       = 0x41
	OpFoundSources      = 0x42
	OpUsersList         = 0x43
	OpFoundSourcesObfu  = 0x44
)

// Tag names used in login (CT_*).
const (
	CTName         = 0x01
	CTPort         = 0x0F
	CTVersion      = 0x11
	CTServerFlags  = 0x20
	CTEMuleVersion = 0xFB
)

// Capability flags sent to the server in CT_SERVER_FLAGS.
const (
	SrvCapZlib       = 0x0001
	SrvCapIPInLogin  = 0x0002
	SrvCapAuxPort    = 0x0004
	SrvCapNewTags    = 0x0008
	SrvCapUnicode    = 0x0010
	SrvCapLargeFiles = 0x0100
	SrvCapSupportCry = 0x0200
	SrvCapRequestCry = 0x0400
	SrvCapRequireCry = 0x0800
)

// File tag names (FT_*).
const (
	FTFileName     = 0x01
	FTFileSize     = 0x02
	FTFileType     = 0x03
	FTFileFormat   = 0x04
	FTSources      = 0x15
	FTCompleteSrcs = 0x30
	FTFileSizeHi   = 0x3A
	FTMediaArtist  = 0xD0
	FTMediaAlbum   = 0xD1
	FTMediaTitle   = 0xD2
	FTMediaLength  = 0xD3
	FTMediaBitrate = 0xD4
	FTMediaCodec   = 0xD5
)

// Server.met tag names (ST_*).
const (
	STServerName  = 0x01
	STDescription = 0x0B
	STPing        = 0x0C
	STFail        = 0x0D
	STPreference  = 0x0E
	STDynIP       = 0x85
	STMaxUsers    = 0x87
	STSoftFiles   = 0x88
	STHardFiles   = 0x89
	STLastPing    = 0x90
	STVersion     = 0x91
	STUDPFlags    = 0x92
	STAuxPorts    = 0x93
	STLowIDUsers  = 0x94
	STUDPKey      = 0x95
	STUDPKeyIP    = 0x96
	STTCPPortObfu = 0x97
	STUDPPortObfu = 0x98
)
