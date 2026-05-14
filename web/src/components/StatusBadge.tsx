import { Badge } from '@/components/ui/badge'

const styles: Record<string, string> = {
  creating: 'bg-yellow-100 text-yellow-800 hover:bg-yellow-100',
  running:  'bg-green-100 text-green-800 hover:bg-green-100',
  stopped:  'bg-gray-100 text-gray-800 hover:bg-gray-100',
  error:    'bg-red-100 text-red-800 hover:bg-red-100',
}

export function StatusBadge({ status, message }: { status: string; message?: string }) {
  return (
    <Badge className={styles[status] || styles.stopped} title={message || status}>
      {status}
    </Badge>
  )
}
