interface ToggleProps {
  checked: boolean
  description: string
  disabled?: boolean
  label: string
  onChange: (checked: boolean) => void
}

export function Toggle({
  checked,
  description,
  disabled = false,
  label,
  onChange,
}: ToggleProps) {
  return (
    <label className={`dt-toggle${disabled ? ' is-disabled' : ''}`}>
      <span>
        <strong>{label}</strong>
        <small>{description}</small>
      </span>
      <input
        checked={checked}
        disabled={disabled}
        onChange={(event) => onChange(event.target.checked)}
        type="checkbox"
      />
      <span aria-hidden="true" className="dt-toggle__track">
        <span />
      </span>
    </label>
  )
}
