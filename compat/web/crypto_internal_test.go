package web

import (
	"bytes"
	"encoding/hex"
	"github.com/cloudflare/circl/xof/k12"
	"testing"
)

// TestKMACVectors pins KMAC against NIST SP 800-185's own samples — the same
// ones the Web Platform Tests carry. A MAC that is merely self-consistent is
// worthless; these fix it to the standard.
func TestKMACVectors(t *testing.T) {
	key, _ := hex.DecodeString("404142434445464748494a4b4c4d4e4f505152535455565758595a5b5c5d5e5f")
	msg := []byte{0x00, 0x01, 0x02, 0x03}
	for _, tc := range []struct {
		name   string
		bits   int
		custom string
		msg    []byte
		want   string
	}{
		{"KMAC128 sample 1", 128, "", msg,
			"e5780b0d3ea6f7d3a429c5706aa43a00fadbd7d49628839e3187243f456ee14e"},
		{"KMAC128 sample 2", 128, "My Tagged Application", msg,
			"3b1fba963cd8b0b59e8c1a6d71888b7143651af8ba0a7070c0979e2811324aa5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := kmac(tc.bits, key, tc.msg, []byte(tc.custom), 32)
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(got) != tc.want {
				t.Errorf("kmac = %s, want %s", hex.EncodeToString(got), tc.want)
			}
		})
	}
}

// TestOCBVectors pins AES-OCB to the Web Platform Tests' own fixtures, which
// are the RFC 7253 construction applied to a long message across all three key
// sizes and all three tag lengths. This mode is written out here rather than
// taken from the standard library, so the vectors are the only thing standing
// between "it round-trips" and "it is OCB" — a cipher that decrypts its own
// output is not evidence of anything.
func TestOCBVectors(t *testing.T) {
	plaintext, _ := hex.DecodeString("546869732073706563696669636174696f6e206465736372696265732061204a6176615363726970742041504920666f7220706572666f726d696e672062617369632063727970746f67726170686963206f7065726174696f6e7320696e20776562206170706c69636174696f6e732c20737563682061732068617368696e672c207369676e61747572652067656e65726174696f6e20616e6420766572696669636174696f6e2c20616e6420656e6372797074696f6e20616e642064656372797074696f6e2e204164646974696f6e616c6c792c2069742064657363726962657320616e2041504920666f72206170706c69636174696f6e7320746f2067656e657261746520616e642f6f72206d616e61676520746865206b6579696e67206d6174657269616c206e656365737361727920746f20706572666f726d207468657365206f7065726174696f6e732e205573657320666f722074686973204150492072616e67652066726f6d2075736572206f7220736572766963652061757468656e7469636174696f6e2c20646f63756d656e74206f7220636f6465207369676e696e672c20616e642074686520636f6e666964656e7469616c69747920616e6420696e74656772697479206f6620636f6d6d756e69636174696f6e732e")
	iv, _ := hex.DecodeString("3a92732aa6ea39bf3986e0c76c742e")
	aad, _ := hex.DecodeString("5468657265206172652037206675727468657220656469746f7269616c206e6f74657320696e2074686520646f63756d656e742e")
	for _, tc := range []struct {
		key        string
		tagBits    int
		ct         string
		tag        string
		tagEmptyAD string
	}{
		{key: "dec0d4fcbf3c4741c892dabd1cd4c04e", tagBits: 64, ct: "41ddbbd277cf8d6d9061527c3cbdb002fa30401dc5163b60f6d39bb87e744f25a86dbf264d5a4a70de69adbeb6e2f42f82bfcfbe69ba993a84d46c9c6c1e38b4078590a9f978bf10280d9d7faa86c10c2754750d441a0434bc331e37abb5fffc0cee2ec572ff7dab06a2282ad9dd7fa40eabf9690c85f6cda161eed0b31ea1b6866470cd5d6545fefdc9f4430257acf7908d5850ecbcbe49b3fdd38e628d1061a60247e0e82cee52211020e06fbf599d637f1c5faf0f560a2cb47fc8a2258a454e77a560626bd37a15317c3c9c261416c45614b6ad172a2a1ad55415a09c6014ead0b12c706ea5e914387b6e81399e9462f12f9471c60a5010dd5b08c0c2beb7714386eaef146c96536ad1705dc40c8702b197a82ecac5161830694cda15e52d7f6d5d26bdd77b6e176e5ee82ea73205d47845e62395e41873860abb494fc52f05c88ea69f339ac7b2587ff0e48d147abfad427882dfb4da6b86e0fb7fb411c4dfb6e605d9179203fb811ba2df13486c75334d35159e87729d08a650f6a2673d74bc1ec970ef0df42e7bc1ce5b11bc4e5115e2da38b18b11045ea5facfaa8dd3f7c9657ac6796318b162bcc0fc60b56e6f9a90398d7232e4c09a7bf18f4efa9c8c80c3979199822b9a22bbf2f3375b9a95c1d652f9d44f83e6b304fb0687d2", tag: "555c7995756983ec", tagEmptyAD: "1dd03dd5f5674e00"},
		{key: "dec0d4fcbf3c4741c892dabd1cd4c04e", tagBits: 96, ct: "74c34bd46bb35fa14ba0f9e1bbeea50de159df59f7f10b5cc410dab98448c95a97b038a7e34bfbab09e68865a58c901682b22d7728ca49cc3bed132f45227f22c496410688b29fa6eac38b7c47b79abd8e22d1136d959a1a249e6d1fbe274a76a742f51fc23a402ed520333d63ffec865743cdc04d1ed433cd3730613a7a56a521201a6b7a2090c066351bcfa63d08537ba2edd134dc6463fc9d736189620d25428d5e1b8418a106929f40f00cec2dd379c2b88f2348cce7a8e2242d4a2f0e06914477c40621b89b0a675eb4af0d3a7925615095aff0a703fe7a0376bc8920937bf51dbd612c8c893691f1d8a02f8c1e95ef7a673613bb493b22202b23dca7b4dc7c6e3317407f061e266a2d2a6e09b49efbff8112f374fb9f8be56c306978de76dc7f45c6747e9da4a1e178dcfd9e5c9fd6af745039750aae85a6b62bbae90aae5207a1c5a53c4671960f20a50b07a06e3d012dd83381ab9c232e15a015d987dfe7b41ea34bfb970d02a5470c26b9d067848849ad6343a97eb9b43dd8cec045e4bef4e9ac61204adbfe46b1b3c44b9d0b6c5ef0541adbc8ffec6d8bbc6326dd0aad6c192a57010fabc5ffbede151ba296344f523d9c5fe7aaf7bdb15b3e181a4271cf42d065d7126aa1f042d85b0cf732e8f36099e20055eada18d15b2877", tag: "27049adb58b6b61bb87ee245", tagEmptyAD: "6f88de9bd8b87bf7a850f0df"},
		{key: "dec0d4fcbf3c4741c892dabd1cd4c04e", tagBits: 128, ct: "315cd0c5593de2047c2ed779e4d3ea2ae4afec8b30584b51c2f29071316b995b87ddd26678ae6790c9df49dc34754764c81d35560fdf15d0cdeb8f891477c14dca753e14279d5530874c73f127473aafe3be4015405e2e17ec44e4e809adbda26300e46e7e08c9b725acbf956260be5d156cfa5395eb262fcd0f7ebc45c8b2843f740959e36e73fa52297f2c2c5f02bc431f7b17793c0b3992f50e3f83d44627ed94e27ef504aeef407f6cf6d3ac4c81857290f39d557e4365c1cd73939a525c20520728a398873e891d75a452dca9e08f768274d2c02d15cab4fc48df25e09a0e34fc99f3f165e4a9bc5b147731d770b4f4661a7b5efa3f249cfe48e121b46c60902b1a2c93c33a4abc30b2e99eb8fbcf462a7cbf0a8e0d82de6118d80d6fbcd191095120359ff7a56d17d483507d2ced3f686f540c16c330ab01ddac41844b36eb8198f8e351aadc2da8fe6ca6efc047c777fc655a9f85f8d6f55308d9cad71d4549fabae69e26d5b35cb7f1c5ba725a6c00f6c2f27b7ff406a103ee7feaf51e4f5dca675d5a7b2a6985e6509fd1c67a99c826017759282aa2b7054d0aa18f80fb680484acfc929005b71f8c361f9bea6e81290c7de7bb31d2c4d5559b8781216047fdf48c7550246becc6aa5d957040a7208f598f97fd763b98f6c59457", tag: "beaa6f0b3fb1957d7c90cadd33f5cbbc", tagEmptyAD: "f6262b4bbfbf58916cbed84729445a69"},
		{key: "d0ee83413f44c43fbad03dcfa61263981d6ddd5ff01e1cf6", tagBits: 64, ct: "e67ed2401bdf7b577a1bf0ffdc09d971d136688f4b4116228fd97d25161de43b7b2327a8d822b21b17026c9c4a3b442548bf67aa050287e3b8d3a499da305a2588795640045291f6ebc6a96be51bf7e687969fb69029b33ea55efec9009a4141d84806a8490c4a8096a826a33e4db35878bf46b9bc0e5b74b9808796327bc89f641127fc1ccdb2215926347b3e553563078ffbc223997faaaac4311aad05be0fb92203fea95ffe6d6b07fe95bd36ac155939fa8e6b348f1953f27da46c7b8726488b2143f61647d80180eef4b7906ab198a9c16139da7935c8553d1a9cb94a8e0cb6703cd61f93684a32442f01f595d01cd549f4d866415e40d8e0ec65701bc5ad18397163b18e01ab48feab16469e9726dbbe6b76781227c8112cfa794e312cbbdf8696cf7bfad1fa5bcb83ec8352a5aff608376287698dcf83a95c94ea0d21d77e794faee52a87acc71d8efe49936e526b497a266a851c2163738e49667ac297f368c87a34c9b405c3df13ddc3263eee42c5b408c2f98bb857cdcf4fa0eb89c1513114dae2f920c5c18fad879298c6bd1550a1e1142c1c5797deb4bf93594f9f2e08aef2ad363a6c70335f5a8ca232a2f2efdcbe4c4e079c19bec3478c20856230cb91d6cfd46dba0c7ad158c9e1f7e459d97c28882869362df322ca7a37", tag: "64aaab03d1ad1d09", tagEmptyAD: "a9e41948ce4ced3c"},
		{key: "d0ee83413f44c43fbad03dcfa61263981d6ddd5ff01e1cf6", tagBits: 96, ct: "840dcbe2c89fff86b2fbc007a88b45970f4ca73ae9706496542d1d5e7a63e629227a1d803e83836a38b255f6953e3a4983455f097d4486656b7fa4404f80d85ed3a38cc5c9ae5f2fd7431c5d7d57059d9d96eb36befdbe9727478abcc437d6c2d8c5c11e123775a8cb7bc554be04870dd46c3b0e08ff31a69dfdd8674a9844b3157c3862020d3885b9ef6cf6ad72c2b79da6aa3df9f0fd4e6bf4fb725ac903400544a446028b1b4be53ca80f6dd95d260e7ae1f571b9253d08993ce10631f86510a237294fbf4f0068a7abaf604574e12c0475ff52a8ff5f3e274dfbd32add5ac08e34f077041a0088bb085c2e45d96b5cc0ce7ae4737be0eb1bcc480ff6f3ac8d81816078a7d0baaa10030a92645f1c29a3d3284dbbd98d938dd765320ae116669473dceb9f8800f5c92dae8c803c39ee214791c3bb62734f46550b0a25591e3bf19615149e0f6f37e38aea71ed105c2055ad4f509701ca9404f704137211533cae0df2ff4d6770dcaf8f49ffa420d1a50711335f626209aa88b3b37ab4733c4c5c7c618622d18aad3490c6d241f0e8c266d739d1f9a2bafb6bc2375d319e8d971d55b2bb49aecec58707754cd03b25a8c4967d522065e52f14e7e0d0cd672788c5084219135b9d7bd58ecf35226492c4b53673f286479940209725b5b383", tag: "eb66616623f7b31430b25668", tagEmptyAD: "2628d32d3c16432100dff9e4"},
		{key: "d0ee83413f44c43fbad03dcfa61263981d6ddd5ff01e1cf6", tagBits: 128, ct: "acde11358e83107094a04fbf05ac81704f5a0aa2f0d1c9cf6434993d25d889238e63d152f44142195233b1d7cdba1ae7234db79e025555362dadab4037a3559cc3e9cab5f204c9c0b851e5044159533a8ce9dc1edd18689b71b3a4c28f646cff859eba51f613d59814b59cdd509b34a56e6e3362da437e464a1965b536203e1626f3dad9e0310fb560b15d24da138e190c14427a4860b645c28fad488428610062ced676ea20b9ad4168dc5180cfba3ce3ab50c443edda67109a123a1d7c56a1d83bcd7a17a616885043e4841464eb7d38e70a1d6ff9017d3bad193c6b7623d35dfcbd8705fba0aa79c6cbf26c9a8f8fa74d1bcaa23adb9fd04a749208e39120e7914e1d6e4ff66ada05ca044f45708b24894d840f1a7dbb036295ac0bc9f2d405f4d23fc5a0d8c2234a74357f0b464519c39d4ba9f0b8ec8368f30c208bf6141695fd89670bc7559158981f6608e2b973d299f16929a441d2f71cb9d81a4a20d014fec9ad2dc0f3979c161d04fa3b0f7d7026dae659b6d8fb8d5641934eabdc7b4f0e1c4923e3d63d765b38b6abbe00c4b7cde0b31d7b4f43f89cfaad5c2059fc1470fadbbb336dad14135895ce6696fb8b8968b31d80a5e40e37b855494ec670898584df31ab6428b16199993256529917e33710efe04aefd05040ee2440", tag: "e42a32eb37fc57c8f947cc8c5a09266d", tagEmptyAD: "296480a0281da7fdc92a63006ece79e5"},
		{key: "67693823fb1d58073f91ece9cc3af910e5532616a4d27b13eb7b74d8000bbf30", tagBits: 64, ct: "2a7ac32ff29765a265ff639ae0c35cf83ee04986da12cf4022bc258b8784b250f49516becd0e7824f149c4e690300a34e3848e25c7c1a7f292ac7dbe6f6b1dda5f0a18a2faed2af06bd6fd0b12eca7f8e9627a4a5e1d5a626b62b29466ee5d1cc5778c3ecdcce554c6a8aa2f617fe8a29c552e025160994317dfc940d70898ee27cdf2dea17d5164a667bf4604187eccccd84dd2277e44d01fdd9915a6f50c8beed715445b3b6acd148aa4d8eb128bee17dca6f736a6b966da4d144591519d203ea7e8c151dd058a5df05e09df602e41a7e71c2f103057efb9481584e3cefb25a63276c5f840815419b84a667e3eafdb45e953b470e76811a2781392e6d3b5bb79976e539858e051eaa7bfb491452a61bd3b202479176a02c6d0c8e8c57f68ee1e3125dc9919246035fd143b7a4832c5100966bebc5f71fffd88f727232b7c5d9f34f80c04e9d8fdaa7b6f4ce02371357ec2ace5d8a3b137e11ff2db3b68545fe6ad8b66d9ceaa4fdc21d8641836e18af1b8b4be416540d154526b63c7bc7e55ed6a55616d987244d60dd7d04de47d24718f2868692aa08ac93aad611f4960dcda3f6583a163246b46e88fe9406d0359504f93dfbab4e863892f8282e3aea183d30506a9d2443198d246d24b2e85188a3f29fe2e5c1d948b967c359a287db8", tag: "58a1300fdaf14f09", tagEmptyAD: "cf64baa62863035f"},
		{key: "67693823fb1d58073f91ece9cc3af910e5532616a4d27b13eb7b74d8000bbf30", tagBits: 96, ct: "086655181219e01b4788ae312c460386328e76be1a86601c1d975ad47c215649f7ac654963331c7b6514baecf9115969308097325d972e716259c1b55b5f4de03e6755e3e1964aba6fd2bf608bdbc5cab99c765fe62bba5c85555a6bb079a419e8164f621525967a971631dd95e4a535a39dda2880f77d742afc508f2c4f3499ddfe73e7ff1df9eb3c95d40e678fef2739b7178bbe92cf54aa751ba06dd976c5380ee46c3b0ba6542ecf576dd471069e31fc70265ffd3b12c37c6c3ce68a67c79e228c023c685d0f7f0dd54852f24633bcdb7b947d00e277da874adb007481e63185bae472aa98dee73c55e5924c225c39a1d36adffb101f2aa3be1343ed4e52f287988c0c22e8d73875e15c1d38f90c882cc373af56b0fbf695114312e2d7347e8c77cd098ab90b15d2d734ea0dab0da847b218afd1a4b65a846858af81193533842be1520c7db10844b8a8874f7fa070127589af87177aee6ee020b1a202c8aa3e8bb760d28416e6c5a6047fba040b937d7b1f47dcce195b87b03796949f498e896db8ac8fdf76dc155c169091ccdea4283890fdc8db469ffb590d1b3ed32c04592206517b8e903f7a4a794262a0e2b2b80d38d4b1a2d66bef704361d364263059372bad4a51f748c41f9884a784e8f049e1fcba6741c55f4fbecac7b61e", tag: "7b4fe3229590a8741d7b8636", tagEmptyAD: "ec8a698b6702e422d029c2cf"},
		{key: "67693823fb1d58073f91ece9cc3af910e5532616a4d27b13eb7b74d8000bbf30", tagBits: 128, ct: "f7b738e6f62ea246f7f6d6aaf301c549bcbcfd4f053feb203d0287ae420d6e4d59f9b4cbe1a17006d93e60deee2a4c51290cbb8ce131c1a5337d4f5f4f8cc135933a49ce4b1cb2ff7cdc8df80b82d3942473428572db295ac9e1f019e9ef6eafe9cfdf0d81f57c4dad133e0716374f389451d5efff1784e1cf82c32d73ed24696a5f3efab7173eac570f8b3c0d080c3cd4450289ce9d0505d058ef2642d159f9e9cceaf3963e27ef069318df59c81c4771526f9e6150a69ec1253df3e2244a626450177402c5a6d70fd4c8279ec06129787ec80a0ec92f969b76057745a97a650035aed5f1716f2de3c099393b94cccb0472ad7a2380564a752bf9f5f5bdde2662a0d3c04123056848e453063f9d68e264cdad43c3ae88977902b90920b14e4576877e85bcaea747911a30177390357c05244cf42074239b6bc1a129517a6f344ad582003c6ad5ce031fa32e59a37d09a6228c3ac51ca16bd6edc0eb713ede307ced6608d7467470c915bc63a800f58d61a3f0eae62a864dfa091e49886c645b36c113f2daae81959538dca88895a5c4c1d609bdb2797deee1965172d7f402ab7b2d53af41787d4162427a08fdb7515d0d4793a3f5c64e8dc80301013c222e342ed5de8fb1ecbc1b0cc4e2db3f7ef511a9531d562ffbcddf61a422bb5289ba", tag: "8ff25e5dd2add40de95e0fbb056fd88e", tagEmptyAD: "1837d4f4203f985b240c4b424591ec82"},
	} {
		key, _ := hex.DecodeString(tc.key)
		wantCT, _ := hex.DecodeString(tc.ct)
		wantTag, _ := hex.DecodeString(tc.tag)
		wantTagEmpty, _ := hex.DecodeString(tc.tagEmptyAD)
		tagLen := tc.tagBits / 8

		st, err := newOCB(key)
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.seal(iv, plaintext, aad, tagLen)
		if err != nil {
			t.Fatalf("seal %d/%d: %v", len(key)*8, tc.tagBits, err)
		}
		want := append(append([]byte{}, wantCT...), wantTag[:tagLen]...)
		if !bytes.Equal(got, want) {
			t.Errorf("seal %d-bit key, %d-bit tag:\n got %X\nwant %X", len(key)*8, tc.tagBits, got, want)
			continue
		}
		// The tag must depend on the associated data, so the same message with
		// no 5468657265206172652037206675727468657220656469746f7269616c206e6f74657320696e2074686520646f63756d656e742e authenticates to something else.
		st2, _ := newOCB(key)
		gotEmpty, err := st2.seal(iv, plaintext, nil, tagLen)
		if err != nil {
			t.Fatal(err)
		}
		wantEmpty := append(append([]byte{}, wantCT...), wantTagEmpty[:tagLen]...)
		if !bytes.Equal(gotEmpty, wantEmpty) {
			t.Errorf("seal (no 5468657265206172652037206675727468657220656469746f7269616c206e6f74657320696e2074686520646f63756d656e742e) %d-bit key, %d-bit tag mismatch", len(key)*8, tc.tagBits)
		}
		st3, _ := newOCB(key)
		back, err := st3.open(iv, want, aad, tagLen)
		if err != nil {
			t.Fatalf("open %d/%d: %v", len(key)*8, tc.tagBits, err)
		}
		if !bytes.Equal(back, plaintext) {
			t.Errorf("open %d/%d round-trip mismatch", len(key)*8, tc.tagBits)
		}
	}
}

// A tampered tag must not verify — the whole point of an authenticated mode.
func TestOCBRejectsTamperedTag(t *testing.T) {
	key, _ := hex.DecodeString("dec0d4fcbf3c4741c892dabd1cd4c04e")
	iv, _ := hex.DecodeString("3a92732aa6ea39bf3986e0c76c742e")
	st, _ := newOCB(key)
	ct, err := st.seal(iv, []byte("hello world"), []byte("aad"), 16)
	if err != nil {
		t.Fatal(err)
	}
	ct[len(ct)-1] ^= 1
	st2, _ := newOCB(key)
	if _, err := st2.open(iv, ct, []byte("aad"), 16); err == nil {
		t.Fatal("open accepted a tampered tag")
	}
}

// The vectors below are RFC 9861's own, extracted from the Web Platform Tests
// that carry them rather than transcribed by hand — a permutation written from a
// specification will happily agree with itself while disagreeing with every
// other implementation, and only real vectors catch that. KT128 is additionally
// cross-checked against CIRCL, which is an independent implementation.

// ptn is the RFC's input pattern: the bytes 00 01 02 .. FA repeating.
func ptn(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

type xofVector struct {
	strength  int // 128 or 256
	input     int // -1 for the empty input, otherwise ptn(input)
	outBits   int
	want      string
	domain    int // TurboSHAKE only; 0 means the default 0x1F
	customLen int // KangarooTwelve only; -1 for none
}

func (v xofVector) msg() []byte {
	if v.input < 0 {
		return nil
	}
	return ptn(v.input)
}

var turboSHAKEVectors = []xofVector{
	{strength: 128, input: -1, outBits: 256, want: "1e415f1c5983aff2169217277d17bb538cd945a397ddec541f1ce41af2c1b74c", domain: 0},
	{strength: 128, input: -1, outBits: 512, want: "1e415f1c5983aff2169217277d17bb538cd945a397ddec541f1ce41af2c1b74c3e8ccae2a4dae56c84a04c2385c03c15e8193bdf58737363321691c05462c8df", domain: 0},
	{strength: 128, input: 1, outBits: 256, want: "55cedd6f60af7bb29a4042ae832ef3f58db7299f893ebb9247247d856958daa9", domain: 0},
	{strength: 128, input: 17, outBits: 256, want: "9c97d036a3bac819db70ede0ca554ec6e4c2a1a4ffbfd9ec269ca6a111161233", domain: 0},
	{strength: 256, input: -1, outBits: 512, want: "367a329dafea871c7802ec67f905ae13c57695dc2c6663c61035f59a18f8e7db11edc0e12e91ea60eb6b32df06dd7f002fbafabb6e13ec1cc20d995547600db0", domain: 0},
	{strength: 256, input: 1, outBits: 512, want: "3e1712f928f8eaf1054632b2aa0a246ed8b0c378728f60bc970410155c28820e90cc90d8a3006aa2372c5c5ea176b0682bf22bae7467ac94f74d43d39b0482e2", domain: 0},
	{strength: 256, input: 17, outBits: 512, want: "b3bab0300e6a191fbe6137939835923578794ea54843f5011090fa2f3780a9e5cb22c59d78b40a0fbff9e672c0fbe0970bd2c845091c6044d687054da5d8e9c7", domain: 0},
}

var kangarooTwelveVectors = []xofVector{
	{strength: 128, input: -1, outBits: 256, want: "1ac2d450fc3b4205d19da7bfca1b37513c0803577ac7167f06fe2ce1f0ef39e5", customLen: -1},
	{strength: 128, input: -1, outBits: 512, want: "1ac2d450fc3b4205d19da7bfca1b37513c0803577ac7167f06fe2ce1f0ef39e54269c056b8c82e48276038b6d292966cc07a3d4645272e31ff38508139eb0a71", customLen: -1},
	{strength: 128, input: 1, outBits: 256, want: "2bda92450e8b147f8a7cb629e784a058efca7cf7d8218e02d345dfaa65244a1f", customLen: -1},
	{strength: 128, input: 17, outBits: 256, want: "6bf75fa2239198db4772e36478f8e19b0f371205f6a9a93a273f51df37122888", customLen: -1},
	{strength: 128, input: -1, outBits: 256, want: "fab658db63e94a246188bf7af69a133045f46ee984c56e3c3328caaf1aa1a583", customLen: 1},
	{strength: 128, input: 8191, outBits: 256, want: "1b577636f723643e990cc7d6a659837436fd6a103626600eb8301cd1dbe553d6", customLen: -1},
	{strength: 128, input: 8192, outBits: 256, want: "48f256f6772f9edfb6a8b661ec92dc93b95ebd05a08a17b39ae3490870c926c3", customLen: -1},
	{strength: 128, input: 8192, outBits: 256, want: "3ed12f70fb05ddb58689510ab3e4d23c6c6033849aa01e1d8c220a297fedcd0b", customLen: 8189},
	{strength: 128, input: 8192, outBits: 256, want: "6a7c1b6a5cd0d8c9ca943a4a216cc64604559a2ea45f78570a15253d67ba00ae", customLen: 8190},
	{strength: 256, input: -1, outBits: 512, want: "b23d2e9cea9f4904e02bec06817fc10ce38ce8e93ef4c89e6537076af8646404e3e8b68107b8833a5d30490aa33482353fd4adc7148ecb782855003aaebde4a9", customLen: -1},
	{strength: 256, input: -1, outBits: 1024, want: "b23d2e9cea9f4904e02bec06817fc10ce38ce8e93ef4c89e6537076af8646404e3e8b68107b8833a5d30490aa33482353fd4adc7148ecb782855003aaebde4a9b0925319d8ea1e121a609821ec19efea89e6d08daee1662b69c840289f188ba860f55760b61f82114c030c97e5178449608ccd2cd2d919fc7829ff69931ac4d0", customLen: -1},
	{strength: 256, input: 1, outBits: 512, want: "0d005a194085360217128cf17f91e1f71314efa5564539d444912e3437efa17f82db6f6ffe76e781eaa068bce01f2bbf81eacb983d7230f2fb02834a21b1ddd0", customLen: -1},
	{strength: 256, input: 17, outBits: 512, want: "1ba3c02b1fc514474f06c8979978a9056c8483f4a1b63d0dccefe3a28a2f323e1cdcca40ebf006ac76ef0397152346837b1277d3e7faa9c9653b19075098527b", customLen: -1},
	{strength: 256, input: -1, outBits: 512, want: "9280f5cc39b54a5a594ec63de0bb99371e4609d44bf845c2f5b8c316d72b159811f748f23e3fabbe5c3226ec96c62186df2d33e9df74c5069ceecbb4dd10eff6", customLen: 1},
	{strength: 256, input: 8191, outBits: 512, want: "3081434d93a4108d8d8a3305b89682cebedc7ca4ea8a3ce869fbb73cbe4a58eef6f24de38ffc170514c70e7ab2d01f03812616e863d769afb3753193ba045b20", customLen: -1},
	{strength: 256, input: 8192, outBits: 512, want: "c6ee8e2ad3200c018ac87aaa031cdac22121b412d07dc6e0dccbb53423747e9a1c18834d99df596cf0cf4b8dfafb7bf02d139d0c9035725adc1a01b7230a41fa", customLen: -1},
	{strength: 256, input: 8192, outBits: 512, want: "74e47879f10a9c5d11bd2da7e194fe57e86378bf3c3f7448eff3c576a0f18c5caae0999979512090a7f348af4260d4de3c37f1ecaf8d2c2c96c1d16c64b12496", customLen: 8189},
	{strength: 256, input: 8192, outBits: 512, want: "f4b5908b929ffe01e0f79ec2f21243d41a396b2e7303a6af1d6399cd6c7a0a2dd7c4f607e8277f9c9b1cb4ab9ddc59d4b92d1fc7558441f1832c3279a4241b8b", customLen: 8190},
}

func TestTurboSHAKEVectors(t *testing.T) {
	for _, v := range turboSHAKEVectors {
		domain := byte(0x1f)
		if v.domain != 0 {
			domain = byte(v.domain)
		}
		got := turboSHAKE(v.strength/4, domain, v.msg(), v.outBits/8)
		if hex.EncodeToString(got) != v.want {
			t.Errorf("TurboSHAKE%d(ptn(%d), %d bits, D=%#x) = %s, want %s",
				v.strength, v.input, v.outBits, domain, hex.EncodeToString(got), v.want)
		}
	}
}

func TestKangarooTwelveVectors(t *testing.T) {
	for _, v := range kangarooTwelveVectors {
		var custom []byte
		if v.customLen > 0 {
			custom = ptn(v.customLen)
		}
		got := kangarooTwelve(v.strength/4, v.msg(), custom, v.outBits/8)
		if hex.EncodeToString(got) != v.want {
			t.Errorf("KT%d(ptn(%d), %d bits, C=ptn(%d)) = %s, want %s",
				v.strength, v.input, v.outBits, v.customLen, hex.EncodeToString(got), v.want)
		}
	}
}

// TestKangarooTwelveAgainstCIRCL crosses the tree hash with an independent
// implementation at lengths that straddle the 8192-byte chunk boundary, which is
// where a tree hash and a plain sponge stop agreeing.
func TestKangarooTwelveAgainstCIRCL(t *testing.T) {
	for _, n := range []int{0, 1, 8191, 8192, 8193, 16384, 16385, 40000} {
		for _, clen := range []int{0, 7, 100} {
			msg, custom := ptn(n), ptn(clen)
			want := make([]byte, 64)
			h := k12.NewDraft10(custom)
			h.Write(msg)
			h.Read(want)

			got := kangarooTwelve(32, msg, custom, 64)
			if !bytes.Equal(got, want) {
				t.Fatalf("KT128(ptn(%d), C=ptn(%d)):\n got %x\nwant %x", n, clen, got, want)
			}
		}
	}
}
