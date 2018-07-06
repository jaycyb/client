// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package engine

import (
	"bytes"
	"io"

	"github.com/keybase/client/go/libkb"
	keybase1 "github.com/keybase/client/go/protocol/keybase1"
	"github.com/keybase/saltpack"
	saltpackBasic "github.com/keybase/saltpack/basic"
)

type SaltpackDecryptArg struct {
	Source io.Reader
	Sink   io.WriteCloser
	Opts   keybase1.SaltpackDecryptOptions
}

// SaltpackDecrypt decrypts data read from a source into a sink.
type SaltpackDecrypt struct {
	libkb.Contextified
	arg          *SaltpackDecryptArg
	res          keybase1.SaltpackEncryptedMessageInfo
	pnymResolver saltpack.SymmetricKeyResolver
}

// NewSaltpackDecrypt creates a SaltpackDecrypt engine.
func NewSaltpackDecrypt(g *libkb.GlobalContext, arg *SaltpackDecryptArg, pnymResolver saltpack.SymmetricKeyResolver) *SaltpackDecrypt {
	return &SaltpackDecrypt{
		Contextified: libkb.NewContextified(g),
		arg:          arg,
		pnymResolver: pnymResolver,
	}
}

// Name is the unique engine name.
func (e *SaltpackDecrypt) Name() string {
	return "SaltpackDecrypt"
}

// GetPrereqs returns the engine prereqs.
func (e *SaltpackDecrypt) Prereqs() Prereqs {
	return Prereqs{}
}

// RequiredUIs returns the required UIs.
func (e *SaltpackDecrypt) RequiredUIs() []libkb.UIKind {
	return []libkb.UIKind{
		libkb.SaltpackUIKind,
		libkb.SecretUIKind,
	}
}

// SubConsumers returns the other UI consumers for this engine.
func (e *SaltpackDecrypt) SubConsumers() []libkb.UIConsumer {
	return []libkb.UIConsumer{
		&SaltpackSenderIdentify{},
	}
}

func (e *SaltpackDecrypt) promptForDecrypt(m libkb.MetaContext, publicKey keybase1.KID, isAnon bool) (err error) {
	defer m.CTrace("SaltpackDecrypt#promptForDecrypt", func() error { return err })()

	spsiArg := SaltpackSenderIdentifyArg{
		isAnon:           isAnon,
		publicKey:        publicKey,
		interactive:      e.arg.Opts.Interactive,
		forceRemoteCheck: e.arg.Opts.ForceRemoteCheck,
		reason: keybase1.IdentifyReason{
			Reason: "Identify who encrypted this message",
			Type:   keybase1.IdentifyReasonType_DECRYPT,
		},
	}

	spsiEng := NewSaltpackSenderIdentify(e.G(), &spsiArg)
	if err = RunEngine2(m, spsiEng); err != nil {
		return err
	}

	arg := keybase1.SaltpackPromptForDecryptArg{
		Sender: spsiEng.Result(),
	}
	e.res.Sender = arg.Sender

	usedDelegateUI := false
	if e.G().UIRouter != nil {
		if ui, err := e.G().UIRouter.GetIdentifyUI(); err == nil && ui != nil {
			usedDelegateUI = true
		}
	}

	err = m.UIs().SaltpackUI.SaltpackPromptForDecrypt(m.Ctx(), arg, usedDelegateUI)
	if err != nil {
		return err
	}
	return err
}

func (e *SaltpackDecrypt) makeMessageInfo(me *libkb.User, mki *saltpack.MessageKeyInfo) {
	if mki == nil || me == nil {
		return
	}
	ckf := me.GetComputedKeyFamily()
	for _, nr := range mki.NamedReceivers {
		kid := keybase1.KIDFromRawKey(nr, byte(libkb.KIDNaclDH))
		if dev, _ := ckf.GetDeviceForKID(kid); dev != nil {
			edev := dev.ProtExport()
			edev.EncryptKey = kid
			e.res.Devices = append(e.res.Devices, *edev)
		}
	}
	e.res.NumAnonReceivers = mki.NumAnonReceivers
	e.res.ReceiverIsAnon = mki.ReceiverIsAnon
}

func addToKeyring(keyring *saltpackBasic.Keyring, key *libkb.NaclDHKeyPair) {
	keyring.ImportBoxKey((*[libkb.NaclDHKeysize]byte)(&key.Public), (*[libkb.NaclDHKeysize]byte)(key.Private))
}

// Run starts the engine.
func (e *SaltpackDecrypt) Run(m libkb.MetaContext) (err error) {
	defer m.CTrace("SaltpackDecrypt::Run", func() error { return err })()

	// We don't load this in the --paperkey case.
	var me *libkb.User

	var keyring *saltpackBasic.Keyring
	keyring = saltpackBasic.NewKeyring()

	if e.arg.Opts.UsePaperKey {
		// Prompt the user for a paper key. This doesn't require you to be
		// logged in.
		keypair, _, err := getPaperKey(m, nil)
		if err != nil {
			return err
		}
		encryptionNaclKeyPair := keypair.EncryptionKey().(libkb.NaclDHKeyPair)
		addToKeyring(keyring, &encryptionNaclKeyPair)
	} else {
		// This does require you to be logged in.
		if !e.G().ActiveDevice.HaveKeys() {
			return libkb.LoginRequiredError{}
		}

		// Get the device encryption key and per user keys.
		var key *libkb.NaclDHKeyPair
		var err error
		key, err = m.G().ActiveDevice.NaclEncryptionKey()
		if err != nil {
			return err
		}
		// TODO is it ok to add a device key in the logs?
		e.G().Log.CDebugf(m.Ctx(), "adding device key for decryption: %v", key.GetKID())
		addToKeyring(keyring, key)

		perUserKeyring, err := m.G().GetPerUserKeyring()
		if err != nil {
			return err
		}
		pukGen := perUserKeyring.CurrentGeneration()
		for i := 1; i <= int(pukGen); i++ {
			key, err = perUserKeyring.GetEncryptionKeyByGeneration(m, keybase1.PerUserKeyGeneration(i))
			// TODO as above, can we aff the KID to the logs?
			e.G().Log.Warning("adding per user key at generation %v for decryption: %v", i, key.GetKID())
			if err != nil {
				return err
			}
			addToKeyring(keyring, key)
		}
	}

	// For DH mode.
	hookMki := func(mki *saltpack.MessageKeyInfo) error {
		kidToIdentify := libkb.BoxPublicKeyToKeybaseKID(mki.SenderKey)
		return e.promptForDecrypt(m, kidToIdentify, mki.SenderIsAnon)
	}

	// For signcryption mode.
	hookSenderSigningKey := func(senderSigningKey saltpack.SigningPublicKey) error {
		kidToIdentify := libkb.SigningPublicKeyToKeybaseKID(senderSigningKey)
		// See if the sender signing key is nil or all zeroes.
		isAnon := false
		if senderSigningKey == nil || bytes.Equal(senderSigningKey.ToKID(), make([]byte, len(senderSigningKey.ToKID()))) {
			isAnon = true
		}
		return e.promptForDecrypt(m, kidToIdentify, isAnon)
	}

	e.G().Log.Debug("| SaltpackDecrypt")
	var mki *saltpack.MessageKeyInfo
	mki, err = libkb.SaltpackDecrypt(m.Ctx(), m.G(), e.arg.Source, e.arg.Sink, keyring, hookMki, hookSenderSigningKey, e.pnymResolver)
	if err == saltpack.ErrNoDecryptionKey {
		err = libkb.NoDecryptionKeyError{Msg: "no suitable device key found"}
	}

	// Since messages recipients are never public any more, this is only meaningful for messages generated by
	// very old clients (or potentially saltpack messages generated for a keybase user by some other app).
	// It's ok if me is nil here.
	e.makeMessageInfo(me, mki)

	return err
}

func (e *SaltpackDecrypt) MessageInfo() keybase1.SaltpackEncryptedMessageInfo {
	return e.res
}
