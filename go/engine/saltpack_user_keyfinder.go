// Copyright 2015 Keybase, Inc. All rights reserved. Use of
// this source code is governed by the included BSD license.

package engine

import (
	"fmt"

	"github.com/keybase/client/go/libkb"

	keybase1 "github.com/keybase/client/go/protocol/keybase1"
)

// SaltpackUserKeyfinder is an engine to find Per User Keys (PUK). Users can also be loaded by assertions, possibly tracking them if necessary.
// This engine does not find per team keys, which capability is implemented by SaltpackRecipientKeyfinder in the saltpackKeyHelpers package.
type SaltpackUserKeyfinder struct {
	libkb.Contextified
	Arg                           libkb.SaltpackRecipientKeyfinderArg
	RecipientEntityKeyMap         map[keybase1.UserOrTeamID]([]keybase1.KID)
	RecipientDeviceAndPaperKeyMap map[keybase1.UID]([]keybase1.KID)
}

// NewSaltpackUserKeyfinder creates a SaltpackUserKeyfinder engine.
func NewSaltpackUserKeyfinderAsInterface(g *libkb.GlobalContext, Arg libkb.SaltpackRecipientKeyfinderArg) libkb.SaltpackRecipientKeyfinderEngineInterface {
	return &SaltpackUserKeyfinder{
		Contextified: libkb.NewContextified(g),
		Arg:          Arg,
		RecipientEntityKeyMap:         make(map[keybase1.UserOrTeamID]([]keybase1.KID)),
		RecipientDeviceAndPaperKeyMap: make(map[keybase1.UID]([]keybase1.KID)),
	}
}

func NewSaltpackUserKeyfinder(g *libkb.GlobalContext, Arg libkb.SaltpackRecipientKeyfinderArg) *SaltpackUserKeyfinder {
	return &SaltpackUserKeyfinder{
		Contextified: libkb.NewContextified(g),
		Arg:          Arg,
		RecipientEntityKeyMap:         make(map[keybase1.UserOrTeamID]([]keybase1.KID)),
		RecipientDeviceAndPaperKeyMap: make(map[keybase1.UID]([]keybase1.KID)),
	}
}

// Name is the unique engine name.
func (e *SaltpackUserKeyfinder) Name() string {
	return "SaltpackUserKeyfinder"
}

// Prereqs returns the engine prereqs.
func (e *SaltpackUserKeyfinder) Prereqs() Prereqs {
	return Prereqs{}
}

// RequiredUIs returns the required UIs.
func (e *SaltpackUserKeyfinder) RequiredUIs() []libkb.UIKind {
	return []libkb.UIKind{}
}

// SubConsumers returns the other UI consumers for this engine.
func (e *SaltpackUserKeyfinder) SubConsumers() []libkb.UIConsumer {
	return []libkb.UIConsumer{}
}

func (e *SaltpackUserKeyfinder) GetPublicKIDs() []keybase1.KID {
	var r []keybase1.KID
	for _, keys := range e.RecipientDeviceAndPaperKeyMap {
		r = append(r, keys...)
	}
	for _, keys := range e.RecipientEntityKeyMap {
		r = append(r, keys...)
	}

	return r
}

func (e *SaltpackUserKeyfinder) GetSymmetricKeys() []libkb.SaltpackReceiverSymmetricKey {
	return []libkb.SaltpackReceiverSymmetricKey{}
}

func (e *SaltpackUserKeyfinder) Run(m libkb.MetaContext) (err error) {
	defer m.CTrace("SaltpackUserKeyfinder#Run", func() error { return err })()

	if e.Arg.Self != nil && !e.Arg.NoSelfEncrypt {
		var selfUpk *keybase1.UserPlusKeysV2AllIncarnations
		selfUpk, err = e.Arg.Self.ExportToUPKV2AllIncarnations()
		if err != nil {
			return err
		}
		e.AddUserRecipient(m, &selfUpk.Current)
	}

	err = e.lookupRecipients(m)
	if err != nil {
		return err
	}
	return nil
}

// lookupRecipients adds the KID corresponding to each recipient to the recipientMap
func (e *SaltpackUserKeyfinder) lookupRecipients(m libkb.MetaContext) error {
	for _, u := range e.Arg.Recipients {
		err := e.LookupUser(m, u) // For existing users
		if err == nil {
			continue
		} else if _, isIdentifyFailedError := err.(libkb.IdentifyFailedError); !isIdentifyFailedError {
			return err
		}
		return fmt.Errorf("Cannot find keys for %v: it is not an assertion for a registered user (err = %v)", u, err)
	}
	return nil
}

func (e *SaltpackUserKeyfinder) LookupUser(m libkb.MetaContext, user string) error {

	Arg := keybase1.Identify2Arg{
		UserAssertion: user,
		Reason: keybase1.IdentifyReason{
			Type: keybase1.IdentifyReasonType_ENCRYPT,
		},
		AlwaysBlock:      true,
		IdentifyBehavior: keybase1.TLFIdentifyBehavior_CLI,
	}
	eng := NewResolveThenIdentify2(m.G(), &Arg)
	if err := RunEngine2(m, eng); err != nil {
		return libkb.IdentifyFailedError{Assertion: user, Reason: err.Error()}
	}

	engRes := eng.Result()
	if engRes == nil {
		return fmt.Errorf("Null result from Identify2")
	}
	arg := libkb.NewLoadUserByUIDArg(m.Ctx(), e.G(), engRes.Upk.GetUID())
	upak, _, err := e.G().GetUPAKLoader().LoadV2(arg)
	if err != nil {
		return err
	}

	return e.AddUserRecipient(m, &upak.Current)
}

func (e *SaltpackUserKeyfinder) hasRecipientDeviceOrPaperKeys(id keybase1.UID) bool {
	_, ok := e.RecipientDeviceAndPaperKeyMap[id]
	return ok
}

func (e *SaltpackUserKeyfinder) hasRecipientEntityKeys(id keybase1.UserOrTeamID) bool {
	_, ok := e.RecipientEntityKeyMap[id]
	return ok
}

func (e *SaltpackUserKeyfinder) AddUserRecipient(m libkb.MetaContext, upk *keybase1.UserPlusKeysV2) error {
	if err := e.AddDeviceAndPaperKeys(m, upk); err != nil {
		return err
	}
	return e.AddPUK(m, upk)
}

func (e *SaltpackUserKeyfinder) AddDeviceAndPaperKeys(m libkb.MetaContext, upk *keybase1.UserPlusKeysV2) error {
	if !e.Arg.UsePaperKeys && !e.Arg.UseDeviceKeys {
		// No need to add anything
		return nil
	}

	if e.hasRecipientDeviceOrPaperKeys(upk.Uid) {
		// This user's keys were already added
		return nil
	}

	var keys []keybase1.KID
	hasPaperKey := false
	hasDeviceKey := false

	hasPUK := len(upk.PerUserKeys) > 0

	for KID, key := range upk.DeviceKeys {
		// Note: for Nacl encryption keys, the DeviceType field is not set, so we need to look at the "parent" signing key
		if libkb.KIDIsDeviceEncrypt(KID) && key.Parent != nil && libkb.IsPaperKey(upk.DeviceKeys[*key.Parent]) {
			hasPaperKey = true
			if e.Arg.UsePaperKeys {
				keys = append(keys, KID)
				// TODO Is it ok to put recipient names in the logs?
				e.G().Log.CDebugf(m.Ctx(), "adding user %v's paper key", upk.Username)
			}
		}

		if libkb.KIDIsDeviceEncrypt(KID) && key.Parent != nil && !libkb.IsPaperKey(upk.DeviceKeys[*key.Parent]) {
			hasDeviceKey = true
			if e.Arg.UseDeviceKeys {
				keys = append(keys, KID)
				// TODO SENSITIVE! Remove from Logs
				e.G().Log.CDebugf(m.Ctx(), "adding user %v's device key", upk.Username)
			}
		}
	}

	if len(keys) == 0 {
		return libkb.NoNaClEncryptionKeyError{
			Username:     upk.Username,
			HasPGPKey:    len(upk.PGPKeys) > 0,
			HasPUK:       hasPUK,
			HasDeviceKey: hasDeviceKey,
			HasPaperKey:  hasPaperKey,
		}
	}

	if e.Arg.UseDeviceKeys && !hasDeviceKey {
		e.G().Log.CWarningf(m.Ctx(), "User %v does not have a device key (they can still decrypt the message with a paper key).", upk.Username)
	}
	if e.Arg.UsePaperKeys && !hasPaperKey {
		e.G().Log.CWarningf(m.Ctx(), "User %v does not have a paper key (they can still decrypt the message with a non paper device key).", upk.Username)
	}

	e.RecipientDeviceAndPaperKeyMap[upk.Uid] = keys

	return nil
}

func (e *SaltpackUserKeyfinder) AddPUK(m libkb.MetaContext, upk *keybase1.UserPlusKeysV2) error {
	if !e.Arg.UseEntityKeys {
		// No need to add anything
		return nil
	}

	if e.hasRecipientDeviceOrPaperKeys(upk.Uid) {
		// This user's keys were already added
		return nil
	}

	var keys []keybase1.KID

	hasPUK := len(upk.PerUserKeys) > 0
	hasPaperKey := false
	hasDeviceKey := false

	if e.Arg.UseEntityKeys {
		maxGen := -1
		var lk keybase1.KID
		for _, k := range upk.PerUserKeys {
			if k.Gen > maxGen {
				maxGen = k.Gen
				lk = k.EncKID
			}
		}
		// TODO SENSITIVE! Remove from Logs
		e.G().Log.CDebugf(m.Ctx(), "adding user %v's latest per user key", upk.Username)
		keys = []keybase1.KID{lk}
	}

	for KID, key := range upk.DeviceKeys {
		if libkb.KIDIsDeviceEncrypt(KID) && key.Parent != nil && libkb.IsPaperKey(upk.DeviceKeys[*key.Parent]) {
			hasPaperKey = true
		}

		if libkb.KIDIsDeviceEncrypt(KID) && key.Parent != nil && !libkb.IsPaperKey(upk.DeviceKeys[*key.Parent]) {
			hasDeviceKey = true
		}
	}

	if len(keys) == 0 {
		return libkb.NoNaClEncryptionKeyError{
			Username:     upk.Username,
			HasPGPKey:    len(upk.PGPKeys) > 0,
			HasPUK:       hasPUK,
			HasDeviceKey: hasDeviceKey,
			HasPaperKey:  hasPaperKey,
		}
	}

	e.RecipientEntityKeyMap[upk.Uid.AsUserOrTeam()] = keys

	return nil
}
