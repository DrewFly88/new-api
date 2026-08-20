/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import {
  Check,
  ChevronDown,
  ChevronRight,
  ChevronsUpDown,
  Code,
  Plus,
  Table,
  Trash2,
} from 'lucide-react'
import { useEffect, useMemo, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'

import { JsonCodeEditor } from '@/components/json-code-editor'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Tabs, TabsContent, TabsList, TabsTrigger } from '@/components/ui/tabs'
import { cn } from '@/lib/utils'

export type ModelMappingOptionGroup = {
  label: string
  /** Direct model items of this group. */
  items?: string[]
  /** Nested sub-groups. Every group level is collapsible. */
  groups?: ModelMappingOptionGroup[]
  /** Collapsed on first open when true. */
  defaultCollapsed?: boolean
  /** Render the group header even when it has no items and no sub-groups. */
  showWhenEmpty?: boolean
}

const GROUP_PATH_SEPARATOR = '\u0000'

type GroupedComboboxInputProps = {
  groups: ModelMappingOptionGroup[]
  value: string
  onValueChange: (value: string) => void
  placeholder?: string
  disabled?: boolean
  ariaLabel?: string
}

/**
 * A free-text input combined with grouped (and nested) suggestions.
 * Supports typing a custom value not present in any group.
 * All group levels are collapsible; searching force-expands them.
 */
function GroupedComboboxInput({
  groups,
  value,
  onValueChange,
  placeholder,
  disabled,
  ariaLabel,
}: GroupedComboboxInputProps) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [highlightedIndex, setHighlightedIndex] = useState(-1)
  // Per-group-path collapse overrides; falls back to defaultCollapsed.
  const [userCollapsed, setUserCollapsed] = useState<Record<string, boolean>>(
    {}
  )
  const containerRef = useRef<HTMLDivElement>(null)
  const inputRef = useRef<HTMLInputElement>(null)

  const normalizedQuery = query.trim().toLowerCase()

  const isExpanded = (
    path: string,
    group: ModelMappingOptionGroup
  ): boolean => {
    // Searching force-expands every group so matches stay visible.
    if (normalizedQuery) return true
    return userCollapsed[path] ?? !group.defaultCollapsed
  }

  const toggleGroup = (path: string, expanded: boolean) => {
    setUserCollapsed((previous) => ({
      ...previous,
      [path]: !expanded,
    }))
  }

  const filterItems = (items: string[] | undefined): string[] => {
    if (!items) return []
    if (!normalizedQuery) return items
    return items.filter((item) =>
      item.toLowerCase().includes(normalizedQuery)
    )
  }

  // Walk the group tree: build visible rendered nodes and the flat list of
  // visible items (DOM order) used for keyboard navigation. Items inside a
  // collapsed group (or a collapsed ancestor) are excluded from both.
  const { renderedNodes, flatItems } = useMemo(() => {
    const flat: string[] = []

    const subtreeCount = (group: ModelMappingOptionGroup): number =>
      filterItems(group.items).length +
      (group.groups ?? []).reduce(
        (sum, sub) => sum + subtreeCount(sub),
        0
      )

    const build = (
      groupList: ModelMappingOptionGroup[],
      parentPath: string,
      depth: number
    ): React.ReactNode[] => {
      const nodes: React.ReactNode[] = []
      for (const group of groupList) {
        const path = parentPath
          ? `${parentPath}${GROUP_PATH_SEPARATOR}${group.label}`
          : group.label
        const items = filterItems(group.items)
        const subNodes = build(group.groups ?? [], path, depth + 1)
        const isEmpty = items.length === 0 && subNodes.length === 0
        // Empty groups are hidden while searching, but may still be shown
        // (e.g. an unfetched "Upstream models" group) otherwise.
        if (isEmpty && !(group.showWhenEmpty && !normalizedQuery)) continue

        const expanded = isExpanded(path, group)
        nodes.push(
          <li
            key={`grp-${path}`}
            role='presentation'
            data-group-label
            style={{ paddingLeft: 6 + depth * 12 }}
            className='text-muted-foreground hover:bg-accent/50 flex cursor-pointer items-center gap-1 rounded-sm py-1 pr-2 text-xs font-medium select-none'
            onMouseDown={(event) => {
              event.preventDefault()
              toggleGroup(path, expanded)
            }}
          >
            {expanded ? (
              <ChevronDown className='size-3.5 shrink-0' aria-hidden='true' />
            ) : (
              <ChevronRight className='size-3.5 shrink-0' aria-hidden='true' />
            )}
            <span className='truncate'>{group.label}</span>
            <span className='font-normal opacity-60'>
              ({subtreeCount(group)})
            </span>
          </li>
        )
        if (!expanded) continue

        for (const item of items) {
          const index = flat.length
          flat.push(item)
          const isSelected = value === item
          nodes.push(
            <li
              key={`item-${path}-${item}`}
              role='option'
              aria-selected={isSelected}
              data-item-index={index}
              data-highlighted={index === highlightedIndex}
              style={{ paddingLeft: 20 + depth * 12 }}
              className={cn(
                'hover:bg-accent/50 relative flex cursor-pointer items-center gap-2 rounded-sm py-1.5 pr-2 text-sm select-none',
                index === highlightedIndex &&
                  'bg-accent text-accent-foreground',
                isSelected && 'font-medium'
              )}
              onMouseEnter={() => setHighlightedIndex(index)}
              onMouseDown={(event) => {
                event.preventDefault()
                commit(item)
              }}
            >
              <Check
                className={cn(
                  'size-4 shrink-0',
                  isSelected ? 'opacity-100' : 'opacity-0'
                )}
              />
              <span className='truncate'>{item}</span>
            </li>
          )
        }
        nodes.push(...subNodes)
      }
      return nodes
    }

    return { renderedNodes: build(groups, '', 0), flatItems: flat }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [groups, normalizedQuery, userCollapsed, value, highlightedIndex])

  const totalItems = flatItems.length

  // Close the dropdown on outside click
  useEffect(() => {
    if (!open) return
    const handleClickOutside = (event: MouseEvent) => {
      if (
        containerRef.current &&
        !containerRef.current.contains(event.target as Node)
      ) {
        setOpen(false)
        setQuery('')
      }
    }
    document.addEventListener('mousedown', handleClickOutside)
    return () => document.removeEventListener('mousedown', handleClickOutside)
  }, [open])

  useEffect(() => {
    setHighlightedIndex(-1)
  }, [query, groups, userCollapsed])

  const commit = (nextValue: string) => {
    onValueChange(nextValue)
    setOpen(false)
    setQuery('')
    inputRef.current?.focus()
  }

  const handleKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    if (!open && (event.key === 'ArrowDown' || event.key === 'ArrowUp')) {
      event.preventDefault()
      setOpen(true)
      return
    }
    if (!open) return

    switch (event.key) {
      case 'ArrowDown':
        event.preventDefault()
        setHighlightedIndex((prev) =>
          prev < totalItems - 1 ? prev + 1 : 0
        )
        break
      case 'ArrowUp':
        event.preventDefault()
        setHighlightedIndex((prev) =>
          prev > 0 ? prev - 1 : totalItems - 1
        )
        break
      case 'Enter':
        event.preventDefault()
        if (highlightedIndex >= 0 && flatItems[highlightedIndex] !== undefined) {
          commit(flatItems[highlightedIndex])
        } else {
          // Free-text input is already synced on change; just close.
          setOpen(false)
          setQuery('')
        }
        break
      case 'Escape':
        event.preventDefault()
        setOpen(false)
        setQuery('')
        break
    }
  }

  // Keep the highlighted item visible in the list
  useEffect(() => {
    if (highlightedIndex < 0) return
    const items = [
      ...(containerRef.current?.querySelectorAll<HTMLElement>(
        '[data-item-index]'
      ) ?? []),
    ]
    items[highlightedIndex]?.scrollIntoView({ block: 'nearest' })
  }, [highlightedIndex, renderedNodes])

  const showDropdown = open && (totalItems > 0 || Boolean(query.trim()))

  return (
    <div ref={containerRef} className='relative'>
      <Input
        ref={inputRef}
        type='text'
        role='combobox'
        aria-expanded={open}
        aria-haspopup='listbox'
        aria-autocomplete='list'
        aria-label={ariaLabel}
        autoComplete='off'
        placeholder={placeholder}
        value={open ? query : value}
        disabled={disabled}
        onChange={(event) => {
          const nextValue = event.target.value
          setQuery(nextValue)
          onValueChange(nextValue)
          if (!open) setOpen(true)
        }}
        onFocus={() => {
          setQuery(value)
          setOpen(true)
        }}
        onKeyDown={handleKeyDown}
        className='pr-9'
      />
      <ChevronsUpDown className='pointer-events-none absolute top-1/2 right-3 size-4 shrink-0 -translate-y-1/2 opacity-50' />

      {showDropdown && (
        <div className='bg-popover text-popover-foreground absolute top-full z-100 mt-1 w-full rounded-md border shadow-md'>
          {totalItems > 0 ? (
            <ul
              role='listbox'
              className='max-h-[240px] overflow-y-auto p-1'
              onMouseDown={(event) => event.preventDefault()}
            >
              {renderedNodes}
            </ul>
          ) : (
            <div className='px-2 py-6 text-center text-sm'>
              {t('No matching models found.')}
              {query.trim() && (
                <div className='text-muted-foreground mt-1 text-xs'>
                  {t('Press Enter to use "{{value}}"', {
                    value: query.trim(),
                  })}
                </div>
              )}
            </div>
          )}
        </div>
      )}
    </div>
  )
}

type ModelMappingEditorProps = {
  value: string
  onChange: (value: string) => void
  disabled?: boolean
  sourceModelGroups?: ModelMappingOptionGroup[]
  targetModelGroups?: ModelMappingOptionGroup[]
}

type MappingRow = {
  id: string
  from: string
  to: string
}

const DUPLICATE_MAPPING_SENTINEL = '{ "duplicate_source_models": '

function getDuplicateSources(rows: MappingRow[]): string[] {
  const seen = new Set<string>()
  const duplicates = new Set<string>()

  for (const row of rows) {
    const source = row.from.trim()
    if (!source) continue
    if (seen.has(source)) {
      duplicates.add(source)
    } else {
      seen.add(source)
    }
  }

  return Array.from(duplicates)
}

export function ModelMappingEditor(props: ModelMappingEditorProps) {
  const { t } = useTranslation()
  const [mode, setMode] = useState<'visual' | 'json'>('visual')
  const [rows, setRows] = useState<MappingRow[]>([])
  const [jsonValue, setJsonValue] = useState(props.value)
  const [jsonError, setJsonError] = useState<string | null>(null)
  const nextRowIdRef = useRef(0)
  const duplicateSources = useMemo(() => getDuplicateSources(rows), [rows])

  const sourceGroups = useMemo<ModelMappingOptionGroup[]>(
    () => props.sourceModelGroups ?? [],
    [props.sourceModelGroups]
  )

  const targetGroups = useMemo<ModelMappingOptionGroup[]>(
    () => props.targetModelGroups ?? [],
    [props.targetModelGroups]
  )

  const createRowId = () => {
    nextRowIdRef.current += 1
    return `mapping-${nextRowIdRef.current}`
  }

  const parseJsonToRows = (json: string): boolean => {
    try {
      if (!json.trim()) {
        setRows([])
        setJsonError(null)
        return true
      }
      const parsed = JSON.parse(json)
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
        setJsonError(t('Model mapping must be a valid JSON object'))
        return false
      }
      const entries = Object.entries(parsed)
      const invalidValue = entries.find(([, to]) => typeof to !== 'string')
      if (invalidValue) {
        setJsonError(t('Model mapping values must be strings'))
        return false
      }
      setRows((previousRows) => {
        const remainingRows = [...previousRows]
        return entries.map(([from, to], index) => {
          const toString = String(to)
          const existingIndex = remainingRows.findIndex(
            (row) =>
              row.from === from ||
              (row.from === from && row.to === toString) ||
              previousRows[index]?.id === row.id
          )
          if (existingIndex >= 0) {
            const [existing] = remainingRows.splice(existingIndex, 1)
            return {
              id: existing.id,
              from,
              to: toString,
            }
          }
          return {
            id: createRowId(),
            from,
            to: toString,
          }
        })
      })
      setJsonError(null)
      return true
    } catch (_error) {
      setJsonError(t('Model mapping must be valid JSON format'))
      return false
    }
  }

  // Parse JSON to rows when value changes externally
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect
    setJsonValue(props.value)
    parseJsonToRows(props.value)
  }, [props.value])

  const convertRowsToJson = (updatedRows: MappingRow[]): string => {
    if (updatedRows.length === 0) {
      return ''
    }
    const obj: Record<string, string> = {}
    updatedRows.forEach((row) => {
      if (row.from.trim()) {
        obj[row.from.trim()] = row.to.trim()
      }
    })
    return JSON.stringify(obj, null, 2)
  }

  const syncRows = (updatedRows: MappingRow[]) => {
    setRows(updatedRows)
    const duplicates = getDuplicateSources(updatedRows)
    if (duplicates.length > 0) {
      setJsonError(t('Duplicate source model mappings are not allowed'))
      setJsonValue(DUPLICATE_MAPPING_SENTINEL)
      props.onChange(DUPLICATE_MAPPING_SENTINEL)
      return
    }

    const json = convertRowsToJson(updatedRows)
    setJsonError(null)
    setJsonValue(json)
    props.onChange(json)
  }

  const handleAddRow = () => {
    const newRow: MappingRow = {
      id: createRowId(),
      from: '',
      to: '',
    }
    syncRows([...rows, newRow])
  }

  const handleDeleteRow = (id: string) => {
    syncRows(rows.filter((row) => row.id !== id))
  }

  const handleRowChange = (
    id: string,
    field: 'from' | 'to',
    newValue: string
  ) => {
    const updatedRows = rows.map((row) =>
      row.id === id ? { ...row, [field]: newValue } : row
    )
    syncRows(updatedRows)
  }

  const handleJsonChange = (newJson: string) => {
    setJsonValue(newJson)
    props.onChange(newJson)
    parseJsonToRows(newJson)
  }

  const handleFillTemplate = () => {
    const template = JSON.stringify(
      { 'gpt-3.5-turbo': 'gpt-3.5-turbo-0125' },
      null,
      2
    )
    setJsonValue(template)
    props.onChange(template)
    parseJsonToRows(template)
  }

  const handleModeChange = (nextMode: string) => {
    if (nextMode !== 'visual' && nextMode !== 'json') return
    if (nextMode === 'json') {
      const duplicates = getDuplicateSources(rows)
      if (duplicates.length === 0) {
        const json = convertRowsToJson(rows)
        setJsonValue(json)
        props.onChange(json)
      }
      setMode('json')
      return
    }
    parseJsonToRows(jsonValue)
    setMode('visual')
  }

  return (
    <div className='space-y-2'>
      <Tabs value={mode} onValueChange={handleModeChange} className='space-y-2'>
        <div className='flex items-center justify-between gap-3'>
          <TabsList>
            <TabsTrigger value='visual'>
              <Table className='h-4 w-4' aria-hidden='true' />
              {t('Visual')}
            </TabsTrigger>
            <TabsTrigger value='json'>
              <Code className='h-4 w-4' aria-hidden='true' />
              {t('JSON')}
            </TabsTrigger>
          </TabsList>
          <Button
            type='button'
            variant='link'
            size='sm'
            className='h-auto p-0'
            onClick={handleFillTemplate}
            disabled={props.disabled}
          >
            {t('Fill Template')}
          </Button>
        </div>

        {jsonError && (
          <Alert variant='destructive'>
            <AlertDescription>{jsonError}</AlertDescription>
          </Alert>
        )}

        {duplicateSources.length > 0 && (
          <Alert>
            <AlertDescription>
              {t('Duplicate source model(s): {{models}}', {
                models: duplicateSources.join(', '),
              })}
            </AlertDescription>
          </Alert>
        )}

        <TabsContent value='visual' className='space-y-2'>
          {rows.length > 0 ? (
            <div className='space-y-2'>
              <div className='grid grid-cols-[1fr_1fr_auto] gap-2 text-sm font-medium'>
                <div>{t('Original Model')}</div>
                <div>{t('Replacement Model')}</div>
                <div className='w-10'></div>
              </div>
              {rows.map((row) => (
                <div
                  key={row.id}
                  className='grid grid-cols-[1fr_1fr_auto] gap-2'
                >
                  <GroupedComboboxInput
                    groups={sourceGroups}
                    value={row.from}
                    onValueChange={(nextValue) =>
                      handleRowChange(row.id, 'from', nextValue)
                    }
                    placeholder='gpt-3.5-turbo'
                    disabled={props.disabled}
                    ariaLabel={t('Original Model')}
                  />
                  <GroupedComboboxInput
                    groups={targetGroups}
                    value={row.to}
                    onValueChange={(nextValue) =>
                      handleRowChange(row.id, 'to', nextValue)
                    }
                    placeholder='gpt-3.5-turbo-0125'
                    disabled={props.disabled}
                    ariaLabel={t('Replacement Model')}
                  />
                  <Button
                    type='button'
                    variant='ghost'
                    size='icon'
                    onClick={() => handleDeleteRow(row.id)}
                    disabled={props.disabled}
                    className='h-10 w-10'
                    aria-label={t('Delete mapping')}
                  >
                    <Trash2 className='h-4 w-4' aria-hidden='true' />
                  </Button>
                </div>
              ))}
            </div>
          ) : (
            <div className='text-muted-foreground flex h-24 items-center justify-center rounded-md border border-dashed text-sm'>
              {t(
                'No model mappings configured. Click "Add Mapping" to get started.'
              )}
            </div>
          )}
          <Button
            type='button'
            variant='outline'
            size='sm'
            onClick={handleAddRow}
            disabled={props.disabled}
            className='w-full'
          >
            <Plus className='mr-2 h-4 w-4' />
            {t('Add Mapping')}
          </Button>
        </TabsContent>
        <TabsContent value='json'>
          <JsonCodeEditor
            value={jsonValue}
            onChange={handleJsonChange}
            placeholder={t('{"original-model": "replacement-model"}')}
            disabled={props.disabled}
            className={jsonError ? 'border-destructive' : undefined}
            aria-invalid={Boolean(jsonError)}
            ariaLabel={t('Model Mapping')}
          />
        </TabsContent>
      </Tabs>
    </div>
  )
}
