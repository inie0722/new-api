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
import { zodResolver } from '@hookform/resolvers/zod'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import type { BaseSyntheticEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { toast } from 'sonner'
import { z } from 'zod'

import { ErrorState } from '@/components/error-state'
import { LoadingState } from '@/components/loading-state'
import { PasswordInput } from '@/components/password-input'
import {
  Form,
  FormControl,
  FormDescription,
  FormField,
  FormItem,
  FormLabel,
  FormMessage,
} from '@/components/ui/form'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from '@/components/ui/select'
import { Switch } from '@/components/ui/switch'
import { api } from '@/lib/api'
import { handleServerError } from '@/lib/handle-server-error'
import { requireServerSuccess } from '@/lib/server-error-message'

import { FormDirtyIndicator } from '../components/form-dirty-indicator'
import { FormNavigationGuard } from '../components/form-navigation-guard'
import {
  SettingsForm,
  SettingsSwitchContent,
  SettingsSwitchItem,
} from '../components/settings-form-layout'
import { SettingsPageFormActions } from '../components/settings-page-context'
import { SettingsSection } from '../components/settings-section'
import { useSettingsForm } from '../hooks/use-settings-form'

const storageSchema = z.object({
  mode: z.enum(['disabled', 'local', 's3']),
  ttl_hours: z.number().int().min(1).max(168),
  local_directory: z.string(),
  endpoint: z.string(),
  region: z.string(),
  bucket: z.string(),
  prefix: z.string(),
  access_key: z.string(),
  secret_key: z.string(),
  path_style: z.boolean(),
})

type StorageConfig = z.infer<typeof storageSchema>
type StorageSettings = {
  config: StorageConfig
  public_address: string
  access_key_configured: boolean
  secret_key_configured: boolean
}
type StorageResponse = {
  success: boolean
  message?: string
  data: StorageSettings
}
const queryKey = ['plugin-file-storage']
const endpoint = '/api/option/plugin_file_storage'

export function PluginFileStorageSection() {
  const { t } = useTranslation()
  const query = useQuery({
    queryKey,
    queryFn: async () =>
      requireServerSuccess((await api.get<StorageResponse>(endpoint)).data)
        .data,
  })
  if (query.isPending) return <LoadingState />
  if (query.isError) return <ErrorState onRetry={() => void query.refetch()} />
  return (
    <SettingsSection title={t('Plugin file storage')}>
      <PluginFileStorageForm settings={query.data} />
    </SettingsSection>
  )
}

function PluginFileStorageForm(props: { settings: StorageSettings }) {
  const { t } = useTranslation()
  const client = useQueryClient()
  const mutation = useMutation({
    mutationFn: async (config: StorageConfig) =>
      requireServerSuccess(
        (await api.put<StorageResponse>(endpoint, config)).data
      ).data,
    onSuccess: (settings) => {
      client.setQueryData(queryKey, settings)
      toast.success(t('Setting updated successfully'))
    },
  })
  const schema = storageSchema
    .extend({
      ttl_hours: z
        .number({
          error: () => t('File URL lifetime must be between 1 and 168 hours'),
        })
        .int(t('File URL lifetime must be between 1 and 168 hours'))
        .min(1, t('File URL lifetime must be between 1 and 168 hours'))
        .max(168, t('File URL lifetime must be between 1 and 168 hours')),
    })
    .superRefine((config, ctx) => {
      const required: Array<
        'local_directory' | 'endpoint' | 'region' | 'bucket'
      > = []
      if (config.mode === 'local') {
        required.push('local_directory')
      }
      if (config.mode === 's3') {
        required.push('endpoint', 'region', 'bucket')
      }
      for (const name of required) {
        if (!config[name].trim()) {
          ctx.addIssue({
            code: 'custom',
            path: [name],
            message: t('This field is required'),
          })
        }
      }
    })
  const { form, handleSubmit, handleReset, isDirty, isSubmitting } =
    useSettingsForm<StorageConfig>({
      resolver: zodResolver(schema),
      defaultValues: props.settings.config,
      onSubmit: async (values) => {
        await mutation.mutateAsync({ ...values })
        // The shared hook uses these submitted values as its new baseline.
        // Erase credentials before it resets the form after a successful save.
        values.access_key = ''
        values.secret_key = ''
      },
    })
  async function save(event?: BaseSyntheticEvent) {
    try {
      await handleSubmit(event)
    } catch (error) {
      handleServerError(error)
    }
  }
  const mode = form.watch('mode')
  const s3Fields = [
    { name: 'endpoint', label: t('S3 endpoint') },
    { name: 'region', label: t('S3 region') },
    { name: 'bucket', label: t('S3 bucket') },
    { name: 'prefix', label: t('S3 prefix') },
  ] as const
  return (
    <>
      <FormNavigationGuard when={isDirty} />
      <Form {...form}>
        <SettingsForm onSubmit={save}>
          <SettingsPageFormActions
            onSave={save}
            onReset={handleReset}
            isSaving={isSubmitting}
            isResetDisabled={!isDirty}
          />
          <FormDirtyIndicator isDirty={isDirty} />
          <p className='text-muted-foreground text-sm'>
            {t(
              'Store uploaded plugin files as temporary URLs for upstream providers'
            )}
          </p>
          <FormField
            control={form.control}
            name='mode'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('Storage mode')}</FormLabel>
                <Select
                  value={field.value}
                  onValueChange={field.onChange}
                  items={{
                    disabled: t('Disabled'),
                    local: t('Local storage'),
                    s3: 'S3',
                  }}
                >
                  <FormControl>
                    <SelectTrigger>
                      <SelectValue />
                    </SelectTrigger>
                  </FormControl>
                  <SelectContent>
                    <SelectItem value='disabled'>{t('Disabled')}</SelectItem>
                    <SelectItem value='local'>{t('Local storage')}</SelectItem>
                    <SelectItem value='s3'>S3</SelectItem>
                  </SelectContent>
                </Select>
                <FormMessage />
              </FormItem>
            )}
          />
          <FormField
            control={form.control}
            name='ttl_hours'
            render={({ field }) => (
              <FormItem>
                <FormLabel>{t('File URL lifetime (hours)')}</FormLabel>
                <FormControl>
                  <Input
                    type='number'
                    min={1}
                    max={168}
                    step={1}
                    {...field}
                    value={Number.isNaN(field.value) ? '' : field.value}
                    onChange={(event) =>
                      field.onChange(event.target.valueAsNumber)
                    }
                  />
                </FormControl>
                <FormDescription>
                  {t('Default: 24 hours. Changes only affect new files.')}
                </FormDescription>
                <FormMessage />
              </FormItem>
            )}
          />
          {mode === 'local' && (
            <>
              <FormField
                control={form.control}
                name='local_directory'
                render={({ field }) => (
                  <FormItem>
                    <FormLabel>{t('Local storage directory')}</FormLabel>
                    <FormControl>
                      <Input {...field} />
                    </FormControl>
                    <FormMessage />
                  </FormItem>
                )}
              />
              <div className='grid gap-2'>
                <Label htmlFor='plugin-file-public-address'>
                  {t('Public file address')}
                </Label>
                <Input
                  id='plugin-file-public-address'
                  readOnly
                  value={props.settings.public_address}
                />
                <p className='text-muted-foreground text-sm'>
                  {t(
                    'Uses the task public address, or the server address when unset. Upstream providers must be able to reach it.'
                  )}
                </p>
              </div>
              <p className='text-muted-foreground text-sm'>
                {t(
                  'Multiple instances must share the local directory and signing secret.'
                )}
              </p>
            </>
          )}
          {mode === 's3' && (
            <>
              {s3Fields.map((item) => (
                <FormField
                  key={item.name}
                  control={form.control}
                  name={item.name}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>{item.label}</FormLabel>
                      <FormControl>
                        <Input {...field} />
                      </FormControl>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              ))}
              {(['access_key', 'secret_key'] as const).map((name) => (
                <FormField
                  key={name}
                  control={form.control}
                  name={name}
                  render={({ field }) => (
                    <FormItem>
                      <FormLabel>
                        {name === 'access_key'
                          ? t('S3 access key')
                          : t('S3 secret key')}
                      </FormLabel>
                      <FormControl>
                        <PasswordInput {...field} autoComplete='new-password' />
                      </FormControl>
                      <FormDescription>
                        {props.settings[`${name}_configured`]
                          ? t(
                              'Configured. Leave blank to keep the current credential.'
                            )
                          : t('Not configured')}
                      </FormDescription>
                      <FormMessage />
                    </FormItem>
                  )}
                />
              ))}
              <FormField
                control={form.control}
                name='path_style'
                render={({ field }) => (
                  <SettingsSwitchItem>
                    <SettingsSwitchContent>
                      <FormLabel>{t('S3 path-style addressing')}</FormLabel>
                    </SettingsSwitchContent>
                    <FormControl>
                      <Switch
                        checked={field.value}
                        onCheckedChange={field.onChange}
                      />
                    </FormControl>
                    <FormMessage />
                  </SettingsSwitchItem>
                )}
              />
              <p className='text-muted-foreground text-sm'>
                {t(
                  'Use a private bucket with read, write, list, and delete permissions for the plugin file prefix.'
                )}
              </p>
            </>
          )}
          <p className='text-muted-foreground text-sm'>
            {t(
              'Changing the directory, bucket, prefix, or credentials may break existing links and prevent cleanup of old files. Wait for files to expire before changing them.'
            )}
          </p>
        </SettingsForm>
      </Form>
    </>
  )
}
