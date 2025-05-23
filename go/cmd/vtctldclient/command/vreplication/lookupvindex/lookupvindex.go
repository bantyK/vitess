/*
Copyright 2023 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package lookupvindex

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"vitess.io/vitess/go/cmd/vtctldclient/cli"
	"vitess.io/vitess/go/cmd/vtctldclient/command/vreplication/common"

	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vschemapb "vitess.io/vitess/go/vt/proto/vschema"
	vtctldatapb "vitess.io/vitess/go/vt/proto/vtctldata"
	topoprotopb "vitess.io/vitess/go/vt/topo/topoproto"
)

// vindexParams is used to unmarshal content from params-file.
type vindexParams struct {
	LookupVindexType  string   `json:"lookup_vindex_type"`
	TableOwner        string   `json:"table_owner"`
	TableOwnerColumns []string `json:"table_owner_columns"`
	TableName         string   `json:"table_name"`
	TableVindexType   string   `json:"table_vindex_type"`
	IgnoreNulls       bool     `json:"ignore_nulls"`
}

var (
	tabletTypesDefault = []topodatapb.TabletType{
		topodatapb.TabletType_REPLICA,
		topodatapb.TabletType_PRIMARY,
	}

	baseOptions = struct {
		// This is where the lookup table and VReplicaiton workflow
		// will be created.
		TableKeyspace string
		// This will be the name of the Lookup Vindex and the name
		// of the VReplication workflow.
		Name    string
		Vschema *vschemapb.Keyspace
	}{}

	// base is the base command for all actions related to Lookup Vindexes.
	base = &cobra.Command{
		Use:                   "LookupVindex --name <name> --table-keyspace <keyspace> [command] [command-flags]",
		Short:                 "Perform commands related to creating, backfilling, and externalizing Lookup Vindexes using VReplication workflows.",
		DisableFlagsInUseLine: true,
		Aliases:               []string{"lookupvindex"},
		Args:                  cobra.NoArgs,
	}

	createOptions = struct {
		Keyspace                     string
		Type                         string
		TableOwner                   string
		TableOwnerColumns            []string
		TableName                    string
		TableVindexType              string
		Cells                        []string
		TabletTypes                  []topodatapb.TabletType
		TabletTypesInPreferenceOrder bool
		IgnoreNulls                  bool
		ContinueAfterCopyWithOwner   bool
		ParamsFile                   string
	}{}

	externalizeOptions = struct {
		Keyspace string
		Delete   bool
	}{}

	internalizeOptions = struct {
		Keyspace string
	}{}

	completeOptions = struct {
		Keyspace string
	}{}

	parseAndValidateCreate = func(cmd *cobra.Command, args []string) error {
		if createOptions.ParamsFile != "" {
			if createOptions.TableOwner != "" {
				return fmt.Errorf("cannot specify both table-owner and params-file")
			}
			if createOptions.Type != "" {
				return fmt.Errorf("cannot specify both type and params-file")
			}
			if len(createOptions.TableOwnerColumns) != 0 {
				return fmt.Errorf("cannot specify both table-owner-columns and params-file")
			}
			paramsFile, err := os.ReadFile(createOptions.ParamsFile)
			if err != nil {
				return err
			}
			createVindexParams := map[string]*vindexParams{}
			err = json.Unmarshal(paramsFile, &createVindexParams)
			if err != nil {
				return err
			}
			return parseVindexParams(createVindexParams, cmd)
		}
		if createOptions.TableOwner == "" {
			return fmt.Errorf("table-owner is a required flag")
		}
		if createOptions.Type == "" {
			return fmt.Errorf("type is a required flag")
		}
		if len(createOptions.TableOwnerColumns) == 0 {
			return fmt.Errorf("table-owner-columns is a required flag")
		}
		if createOptions.TableName == "" { // Use vindex name
			createOptions.TableName = baseOptions.Name
		}
		if !strings.Contains(createOptions.Type, "lookup") {
			return fmt.Errorf("vindex type must be a lookup vindex")
		}
		baseOptions.Vschema = &vschemapb.Keyspace{
			Vindexes: map[string]*vschemapb.Vindex{
				baseOptions.Name: {
					Type: createOptions.Type,
					Params: map[string]string{
						"table":        baseOptions.TableKeyspace + "." + createOptions.TableName,
						"from":         strings.Join(createOptions.TableOwnerColumns, ","),
						"to":           "keyspace_id",
						"ignore_nulls": fmt.Sprintf("%t", createOptions.IgnoreNulls),
					},
					Owner: createOptions.TableOwner,
				},
			},
			Tables: map[string]*vschemapb.Table{
				createOptions.TableOwner: {
					ColumnVindexes: []*vschemapb.ColumnVindex{
						{
							Name:    baseOptions.Name,
							Columns: createOptions.TableOwnerColumns,
						},
					},
				},
				createOptions.TableName: {
					ColumnVindexes: []*vschemapb.ColumnVindex{
						{
							// If the vindex name/type is empty then we'll fill this in
							// later using the defult for the column types.
							Name:    createOptions.TableVindexType,
							Columns: createOptions.TableOwnerColumns,
						},
					},
				},
			},
		}

		// VReplication specific flags.
		ttFlag := cmd.Flags().Lookup("tablet-types")
		if ttFlag != nil && ttFlag.Changed {
			createOptions.TabletTypes = tabletTypesDefault
		}
		cFlag := cmd.Flags().Lookup("cells")
		if cFlag != nil && cFlag.Changed {
			for i, cell := range createOptions.Cells {
				createOptions.Cells[i] = strings.TrimSpace(cell)
			}
		}
		return nil
	}

	parseVindexParams = func(params map[string]*vindexParams, cmd *cobra.Command) error {
		if len(params) == 0 {
			return fmt.Errorf("at least 1 vindex is required")
		}

		vindexes := map[string]*vschemapb.Vindex{}
		tables := map[string]*vschemapb.Table{}
		for vindexName, vindex := range params {
			if len(vindex.TableOwnerColumns) == 0 {
				return fmt.Errorf("table owner columns found empty for '%s'", vindexName)
			}
			if vindex.TableOwner == "" {
				return fmt.Errorf("table owner found empty for '%s'", vindexName)
			}
			if vindex.TableName == "" {
				vindex.TableName = vindexName
			}

			if !strings.Contains(vindex.LookupVindexType, "lookup") {
				return fmt.Errorf("%s is not a lookup vindex type", vindex.LookupVindexType)
			}

			vindexes[vindexName] = &vschemapb.Vindex{
				Type: vindex.LookupVindexType,
				Params: map[string]string{
					"table":        baseOptions.TableKeyspace + "." + vindex.TableName,
					"from":         strings.Join(vindex.TableOwnerColumns, ","),
					"to":           "keyspace_id",
					"ignore_nulls": fmt.Sprintf("%t", vindex.IgnoreNulls),
				},
				Owner: vindex.TableOwner,
			}

			targetTableColumnVindex := &vschemapb.ColumnVindex{
				// If the vindex type is empty then we'll fill this later by
				// choosing the most appropriate vindex type for the given column.
				Name:    vindex.TableVindexType,
				Columns: vindex.TableOwnerColumns,
			}
			sourceTableColumnVindex := &vschemapb.ColumnVindex{
				Name:    vindexName,
				Columns: vindex.TableOwnerColumns,
			}

			if table, ok := tables[vindex.TableName]; !ok {
				tables[vindex.TableName] = &vschemapb.Table{
					ColumnVindexes: []*vschemapb.ColumnVindex{targetTableColumnVindex},
				}
			} else {
				table.ColumnVindexes = append(table.ColumnVindexes, targetTableColumnVindex)
			}

			if table, ok := tables[vindex.TableOwner]; !ok {
				tables[vindex.TableOwner] = &vschemapb.Table{
					ColumnVindexes: []*vschemapb.ColumnVindex{sourceTableColumnVindex},
				}
			} else {
				table.ColumnVindexes = append(table.ColumnVindexes, sourceTableColumnVindex)
			}
		}

		baseOptions.Vschema = &vschemapb.Keyspace{
			Vindexes: vindexes,
			Tables:   tables,
		}

		// VReplication specific flags.
		ttFlag := cmd.Flags().Lookup("tablet-types")
		if ttFlag != nil && ttFlag.Changed {
			createOptions.TabletTypes = tabletTypesDefault
		}
		cFlag := cmd.Flags().Lookup("cells")
		if cFlag != nil && cFlag.Changed {
			for i, cell := range createOptions.Cells {
				createOptions.Cells[i] = strings.TrimSpace(cell)
			}
		}
		return nil
	}

	// cancel makes a WorkflowDelete call to a vtctld.
	cancel = &cobra.Command{
		Use:                   "cancel",
		Short:                 "Cancel the VReplication workflow that backfills the Lookup Vindex.",
		Example:               `vtctldclient --server localhost:15999 LookupVindex --name corder_lookup_vdx --table-keyspace customer cancel`,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		Aliases:               []string{"Cancel"},
		Args:                  cobra.NoArgs,
		RunE:                  commandCancel,
	}

	// complete makes a LookupVindexComplete call to a vtctld.
	complete = &cobra.Command{
		Use:                   "complete",
		Short:                 "Complete the LookupVindex workflow. The Vindex must have been previously externalized. If you want to delete the workflow without externalizing the Vindex then use the cancel command instead.",
		Example:               `vtctldclient --server localhost:15999 LookupVindex --name corder_lookup_vdx --table-keyspace customer complete`,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		Aliases:               []string{"Complete"},
		Args:                  cobra.NoArgs,
		RunE:                  commandComplete,
	}

	// create makes a LookupVindexCreate call to a vtctld.
	create = &cobra.Command{
		Use:                   "create",
		Short:                 "Create the Lookup Vindex(es) in the specified keyspace and backfill them with a VReplication workflow.",
		Example:               `vtctldclient --server localhost:15999 LookupVindex --name corder_lookup_vdx --table-keyspace customer create --keyspace customer --type consistent_lookup_unique --table-owner corder --table-owner-columns sku --table-name corder_lookup_tbl --table-vindex-type unicode_loose_xxhash`,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		Aliases:               []string{"Create"},
		Args:                  cobra.NoArgs,
		PreRunE:               parseAndValidateCreate,
		RunE:                  commandCreate,
	}

	// externalize makes a LookupVindexExternalize call to a vtctld.
	externalize = &cobra.Command{
		Use:                   "externalize",
		Short:                 "Externalize the Lookup Vindex. If the Vindex has an owner the VReplication workflow will also be stopped/deleted.",
		Example:               `vtctldclient --server localhost:15999 LookupVindex --name corder_lookup_vdx --table-keyspace customer externalize`,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		Aliases:               []string{"Externalize"},
		Args:                  cobra.NoArgs,
		RunE:                  commandExternalize,
	}

	// internalize makes a LookupVindexInternalize call to a vtctld.
	internalize = &cobra.Command{
		Use:                   "internalize",
		Short:                 "Internalize the Vindex again to continue the backfill, making it unusable for queries again.",
		Example:               `vtctldclient --server localhost:15999 LookupVindex --name corder_lookup_vdx --table-keyspace customer internalize`,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		Aliases:               []string{"Internalize"},
		Args:                  cobra.NoArgs,
		RunE:                  commandInternalize,
	}

	// show makes a GetWorkflows call to a vtctld.
	show = &cobra.Command{
		Use:                   "show",
		Short:                 "Show the status of the VReplication workflow that backfills the Lookup Vindex.",
		Example:               `vtctldclient --server localhost:15999 LookupVindex --name corder_lookup_vdx --table-keyspace customer show`,
		SilenceUsage:          true,
		DisableFlagsInUseLine: true,
		Aliases:               []string{"Show"},
		Args:                  cobra.NoArgs,
		RunE:                  commandShow,
	}
)

func commandCancel(cmd *cobra.Command, args []string) error {
	cli.FinishedParsing(cmd)

	req := &vtctldatapb.WorkflowDeleteRequest{
		Keyspace: baseOptions.TableKeyspace,
		Workflow: baseOptions.Name,
	}
	_, err := common.GetClient().WorkflowDelete(common.GetCommandCtx(), req)
	if err != nil {
		return err
	}

	output := fmt.Sprintf("LookupVindex %s left in place and the %s VReplication wokflow has been deleted",
		baseOptions.Name, baseOptions.Name)
	fmt.Println(output)

	return nil
}

func commandComplete(cmd *cobra.Command, args []string) error {
	if completeOptions.Keyspace == "" {
		completeOptions.Keyspace = baseOptions.TableKeyspace
	}
	cli.FinishedParsing(cmd)

	_, err := common.GetClient().LookupVindexComplete(common.GetCommandCtx(), &vtctldatapb.LookupVindexCompleteRequest{
		Keyspace: completeOptions.Keyspace,
		// The name of the workflow and lookup vindex.
		Name: baseOptions.Name,
		// Where the lookup table and VReplication workflow were created.
		TableKeyspace: baseOptions.TableKeyspace,
	})

	if err != nil {
		return err
	}

	output := fmt.Sprintf("LookupVindex %s has been completed and the VReplication workflow has been deleted.", baseOptions.Name)
	fmt.Println(output)

	return nil
}

func commandCreate(cmd *cobra.Command, args []string) error {
	tsp := common.GetTabletSelectionPreference(cmd)
	cli.FinishedParsing(cmd)

	_, err := common.GetClient().LookupVindexCreate(common.GetCommandCtx(), &vtctldatapb.LookupVindexCreateRequest{
		Workflow:                   baseOptions.Name,
		Keyspace:                   createOptions.Keyspace,
		Vindex:                     baseOptions.Vschema,
		ContinueAfterCopyWithOwner: createOptions.ContinueAfterCopyWithOwner,
		Cells:                      createOptions.Cells,
		TabletTypes:                createOptions.TabletTypes,
		TabletSelectionPreference:  tsp,
	})

	if err != nil {
		return err
	}

	output := fmt.Sprintf("LookupVindex %s created in the %s keyspace and the %s VReplication wokflow scheduled on the %s shards, use show to view progress",
		baseOptions.Name, createOptions.Keyspace, baseOptions.Name, baseOptions.TableKeyspace)
	fmt.Println(output)

	return nil
}

func commandExternalize(cmd *cobra.Command, args []string) error {
	if externalizeOptions.Keyspace == "" {
		externalizeOptions.Keyspace = baseOptions.TableKeyspace
	}
	cli.FinishedParsing(cmd)

	resp, err := common.GetClient().LookupVindexExternalize(common.GetCommandCtx(), &vtctldatapb.LookupVindexExternalizeRequest{
		Keyspace: externalizeOptions.Keyspace,
		// The name of the workflow and lookup vindex.
		Name: baseOptions.Name,
		// Where the lookup table and VReplication workflow were created.
		TableKeyspace: baseOptions.TableKeyspace,
		// Delete the workflow after externalizing, instead of stopping.
		DeleteWorkflow: externalizeOptions.Delete,
	})

	if err != nil {
		return err
	}

	output := fmt.Sprintf("LookupVindex %s has been externalized", baseOptions.Name)
	if resp.WorkflowStopped {
		output = output + " and the VReplication workflow has been stopped."
	} else if resp.WorkflowDeleted {
		output = output + " and the VReplication workflow has been deleted."
	}
	fmt.Println(output)

	return nil
}

func commandInternalize(cmd *cobra.Command, args []string) error {
	if internalizeOptions.Keyspace == "" {
		internalizeOptions.Keyspace = baseOptions.TableKeyspace
	}
	cli.FinishedParsing(cmd)

	_, err := common.GetClient().LookupVindexInternalize(common.GetCommandCtx(), &vtctldatapb.LookupVindexInternalizeRequest{
		Keyspace: internalizeOptions.Keyspace,
		// The name of the workflow and lookup vindex.
		Name: baseOptions.Name,
		// Where the lookup table and VReplication workflow were created.
		TableKeyspace: baseOptions.TableKeyspace,
	})

	if err != nil {
		return err
	}

	output := fmt.Sprintf("LookupVindex %s has been internalized and the VReplication workflow has been started.", baseOptions.Name)
	fmt.Println(output)

	return nil
}

func commandShow(cmd *cobra.Command, args []string) error {
	cli.FinishedParsing(cmd)

	req := &vtctldatapb.GetWorkflowsRequest{
		Keyspace: baseOptions.TableKeyspace,
		Workflow: baseOptions.Name,
	}
	resp, err := common.GetClient().GetWorkflows(common.GetCommandCtx(), req)
	if err != nil {
		return err
	}

	data, err := cli.MarshalJSONPretty(resp)
	if err != nil {
		return err
	}

	fmt.Printf("%s\n", data)

	return nil
}

func registerCommands(root *cobra.Command) {
	base.PersistentFlags().StringVar(&baseOptions.Name, "name", "", "The name of the Lookup Vindex to create. This will also be the name of the VReplication workflow created to backfill the Lookup Vindex. This will be used only for the workflow name if params-file is used.")
	base.MarkPersistentFlagRequired("name")
	base.PersistentFlags().StringVar(&baseOptions.TableKeyspace, "table-keyspace", "", "The keyspace to create the lookup table in. This is also where the VReplication workflow is created to backfill the Lookup Vindex.")
	base.MarkPersistentFlagRequired("table-keyspace")
	root.AddCommand(base)

	// This will create the lookup vindex in the specified keyspace
	// and setup a VReplication workflow to backfill its lookup table.
	create.Flags().StringVar(&createOptions.Keyspace, "keyspace", "", "The keyspace to create the Lookup Vindex in. This is also where the table-owner must exist.")
	create.MarkFlagRequired("keyspace")
	create.Flags().StringVar(&createOptions.Type, "type", "", "The type of Lookup Vindex to create.")
	create.Flags().StringVar(&createOptions.TableOwner, "table-owner", "", "The table holding the data which we should use to backfill the Lookup Vindex. This must exist in the same keyspace as the Lookup Vindex.")
	create.Flags().StringSliceVar(&createOptions.TableOwnerColumns, "table-owner-columns", nil, "The columns to read from the owner table. These will be used to build the hash which gets stored as the keyspace_id value in the lookup table.")
	create.Flags().StringVar(&createOptions.TableName, "table-name", "", "The name of the lookup table. If not specified, then it will be created using the same name as the Lookup Vindex.")
	create.Flags().StringVar(&createOptions.TableVindexType, "table-vindex-type", "", "The primary vindex name/type to use for the lookup table, if the table-keyspace is sharded. If no value is provided then the default type will be used based on the table-owner-columns types.")
	create.Flags().BoolVar(&createOptions.IgnoreNulls, "ignore-nulls", false, "Do not add corresponding records in the lookup table if any of the owner table's 'from' fields are NULL.")
	create.Flags().BoolVar(&createOptions.ContinueAfterCopyWithOwner, "continue-after-copy-with-owner", true, "Vindex will continue materialization after the backfill completes when an owner is provided.")
	create.Flags().StringVar(&createOptions.ParamsFile, "params-file", "", "JSON file containing lookup vindex parameters. Use this for creating multiple lookup vindexes.")
	// VReplication specific flags.
	create.Flags().StringSliceVar(&createOptions.Cells, "cells", nil, "Cells to look in for source tablets to replicate from.")
	create.Flags().Var((*topoprotopb.TabletTypeListFlag)(&createOptions.TabletTypes), "tablet-types", "Source tablet types to replicate from.")
	create.Flags().BoolVar(&createOptions.TabletTypesInPreferenceOrder, "tablet-types-in-preference-order", true, "When performing source tablet selection, look for candidates in the type order as they are listed in the tablet-types flag.")
	base.AddCommand(create)

	// This will show the output of GetWorkflows client call
	// for the VReplication workflow used.
	base.AddCommand(show)

	// This will also stop the VReplication workflow if the
	// vindex has an owner as the lookup vindex will then be
	// managed by VTGate.
	externalize.Flags().StringVar(&externalizeOptions.Keyspace, "keyspace", "", "The keyspace containing the Lookup Vindex. If no value is specified then the table-keyspace will be used.")
	externalize.Flags().BoolVar(&externalizeOptions.Delete, "delete", false, "Delete the VReplication workflow after externalizing the Vindex, instead of stopping (default false).")
	base.AddCommand(externalize)

	internalize.Flags().StringVar(&internalizeOptions.Keyspace, "keyspace", "", "The keyspace containing the Lookup Vindex. If no value is specified then the table-keyspace will be used.")
	base.AddCommand(internalize)

	complete.Flags().StringVar(&completeOptions.Keyspace, "keyspace", "", "The keyspace containing the Lookup Vindex. If no value is specified then the table-keyspace will be used.")
	base.AddCommand(complete)

	// The cancel command deletes the VReplication workflow used
	// to backfill the lookup vindex. It ends up making a
	// WorkflowDelete VtctldServer call.
	base.AddCommand(cancel)
}

func init() {
	common.RegisterCommandHandler("LookupVindex", registerCommands)
}
