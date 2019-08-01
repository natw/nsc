/*
 * Copyright 2018-2019 The NATS Authors
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package cmd

import (
	"errors"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/nats-io/jwt"
	"github.com/nats-io/nkeys"
	"github.com/nats-io/nsc/cli"
	"github.com/nats-io/nsc/cmd/store"
	"github.com/spf13/cobra"
)

func createMigrateCmd() *cobra.Command {
	var params MigrateCmdParams
	var storeDir string
	var cmd = &cobra.Command{
		Hidden: true,

		Short:   "Migrate an account to the current operator",
		Example: "migrate --url <path or url to account jwt>",
		Use:     `migrate`,
		Args:    MaxArgs(0),
		RunE: func(cmd *cobra.Command, args []string) error {
			if params.url != "" && storeDir != "" {
				return fmt.Errorf("specify one of --url or --store-dir")
			}
			var all []MigrateCmdParams
			if InteractiveFlag {
				ok, err := cli.PromptYN("migrate all accounts under a particular operator")
				if err != nil {
					return err
				}
				if ok {
					storeDir, err = cli.Prompt("specify the directory for the operator", "", true, func(v string) error {
						_, err := store.LoadStore(v)
						return err
					})
					if err != nil {
						return err
					}
				}
			}
			if storeDir != "" {
				if KeyPathFlag != "" {
					return fmt.Errorf("when --store-dir is specified no other options other than '-i' and '--force' are allowed")
				}
				if params.accountKeypath != "" {
					return fmt.Errorf("when --store-dir is specified no other options other than '-i' and '--force' are allowed")
				}

				s, err := store.LoadStore(storeDir)
				if err != nil {
					return fmt.Errorf("error loading store dir %q: %v", storeDir, err)
				}
				names, err := s.ListSubContainers(store.Accounts)
				if err != nil {
					return fmt.Errorf("error listing accounts in %q: %v", storeDir, err)
				}
				for _, n := range names {
					fp := filepath.Join(storeDir, store.Accounts, n, store.JwtName(n))
					var ip MigrateCmdParams
					ip.url = fp
					ip.overwrite = params.overwrite
					all = append(all, ip)
				}
			} else {
				all = append(all, params)
			}

			for _, p := range all {
				// no other args are supported here, and the KeyPathFlag
				// resolution is not re-entrant.
				KeyPathFlag = ""
				if err := RunAction(cmd, []string{}, &p); err != nil {
					return fmt.Errorf("import failed - %v", err)
				}
				m := fmt.Sprintf("migrated %q to operator %q", p.claim.Name, p.operator)
				um := fmt.Sprintf("%d users migrated", len(p.migratedUsers))
				if len(p.migratedUsers) == 0 {
					um = "no users migrated"
				}
				if !p.isFileImport {
					um = ""
				}
				cmd.Printf("%s [%s]\n", m, um)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&params.url, "url", "u", "", "path or url to import jwt from")
	cmd.Flags().StringVarP(&params.accountKeypath, "account-key", "k", "", "path to account key")
	cmd.Flags().StringVarP(&storeDir, "store-dir", "", "", "path to a store dir - all accounts are migrated")
	cmd.Flags().BoolVarP(&params.overwrite, "force", "", false, "overwrite accounts with the same name")
	HoistRootFlags(cmd)
	return cmd
}

func init() {
	GetRootCmd().AddCommand(createMigrateCmd())
}

type MigrateCmdParams struct {
	signer         SignerParams
	accountKeypath string
	accountToken   string
	claim          *jwt.AccountClaims
	url            string
	isFileImport   bool
	operator       string
	migratedUsers  []*jwt.UserClaims
	overwrite      bool
}

func (p *MigrateCmdParams) SetDefaults(ctx ActionCtx) error {
	p.signer.SetDefaults(nkeys.PrefixByteOperator, false, ctx)
	return nil
}

func (p *MigrateCmdParams) PreInteractive(ctx ActionCtx) error {
	var err error
	p.url, err = cli.Prompt("account jwt url/or path ", p.url, true, func(v string) error {
		// we expect either a file or url
		if u, err := url.Parse(v); err == nil && u.Scheme != "" {
			return nil
		}
		v, err := Expand(v)
		if err != nil {
			return err
		}
		_, err = os.Stat(v)
		return err
	})
	if err != nil {
		return err
	}
	return p.signer.Edit(ctx)
}

func (p *MigrateCmdParams) getAccountKeys() []string {
	var keys []string
	keys = append(keys, p.claim.Subject)
	keys = append(keys, p.claim.SigningKeys...)
	return keys
}

func (p *MigrateCmdParams) Load(ctx ActionCtx) error {
	if p.url == "" {
		return errors.New("an url or path to the account jwt is required")
	}
	data, err := LoadFromFileOrURL(p.url)
	if err != nil {
		return fmt.Errorf("error loading from %q: %v", p.url, err)
	}
	p.isFileImport = !IsURL(p.url)

	p.accountToken, err = jwt.ParseDecoratedJWT(data)
	if err != nil {
		return fmt.Errorf("error parsing JWT: %v", err)
	}
	p.claim, err = jwt.DecodeAccountClaims(p.accountToken)
	if err != nil {
		return fmt.Errorf("error decoding JWT: %v", err)
	}

	for _, k := range p.getAccountKeys() {
		kp, _ := ctx.StoreCtx().KeyStore.GetKeyPair(k)
		if kp != nil {
			p.accountKeypath = ctx.StoreCtx().KeyStore.GetKeyPath(k)
			break
		}
	}

	return nil
}

func (p *MigrateCmdParams) PostInteractive(ctx ActionCtx) error {
	var err error
	if p.accountKeypath == "" {
		p.accountKeypath, err = cli.Prompt("account key", "", true,
			SeedNKeyValidatorMatching(nkeys.PrefixByteAccount, p.getAccountKeys()))
		if err != nil {
			return err
		}
	}
	if ctx.StoreCtx().Store.HasAccount(p.claim.Name) && !p.overwrite {
		aac, err := ctx.StoreCtx().Store.ReadAccountClaim(p.claim.Name)
		if err != nil {
			return err
		}
		p.overwrite = aac.Subject == p.claim.Subject
		if !p.overwrite {
			p.overwrite, err = cli.PromptYN("account %q already exists under the current operator, replace it")
		}
	}
	return nil
}

func (p *MigrateCmdParams) Validate(ctx ActionCtx) error {
	if p.isFileImport {
		parent := ctx.StoreCtx().Store.Dir
		// it is already determined to be a file
		fp, err := Expand(p.url)
		if err != nil {
			return err
		}
		if strings.HasPrefix(fp, parent) {
			return fmt.Errorf("cannot migrate %q into %q (same operator)", fp, parent)
		}
	}

	if !p.overwrite && ctx.StoreCtx().Store.HasAccount(p.claim.Name) {
		return fmt.Errorf("account %q already exists, specify --force to overwrite", p.claim.Name)
	}

	var err error
	var kp nkeys.KeyPair
	if p.accountKeypath != "" {
		kp, err = store.ResolveKey(p.accountKeypath)
		if err != nil {
			return err
		}
	}
	pk, err := kp.PublicKey()
	if err != nil {
		return err
	}
	matched := false
	for _, v := range p.getAccountKeys() {
		if v == pk {
			matched = true
			break
		}
	}
	if !matched {
		return fmt.Errorf("unable to match an account public key for: %s", strings.Join(p.getAccountKeys(), ","))
	}
	if err := p.signer.Resolve(ctx); err != nil {
		return err
	}
	return nil
}

func (p *MigrateCmdParams) Run(ctx ActionCtx) error {
	tok, err := p.claim.Encode(p.signer.signerKP)
	if err != nil {
		return err
	}
	p.operator = ctx.StoreCtx().Operator.Name
	if err = ctx.StoreCtx().Store.StoreClaim([]byte(tok)); err != nil {
		return err
	}

	if p.isFileImport {
		udir := filepath.Join(filepath.Dir(p.url), store.Users)
		fi, err := os.Stat(udir)
		if err == nil && fi.IsDir() {
			infos, err := ioutil.ReadDir(udir)
			if err != nil {
				return err
			}
			for _, v := range infos {
				n := v.Name()
				if !v.IsDir() && filepath.Ext(n) == ".jwt" {
					up := filepath.Join(udir, n)
					d, err := Read(up)
					if err != nil {
						return err
					}
					s, err := jwt.ParseDecoratedJWT(d)
					if err != nil {
						return err
					}
					uc, err := jwt.DecodeUserClaims(s)
					if err := ctx.StoreCtx().Store.StoreClaim([]byte(s)); err != nil {
						return err
					}
					p.migratedUsers = append(p.migratedUsers, uc)
				}
			}
		}
	}
	return nil
}