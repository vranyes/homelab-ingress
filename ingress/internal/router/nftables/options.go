package nftables

type MasqueradeRule struct {
	InputDevice  string
	OutputDevice string
}

type options struct {
	masqueradeRules []MasqueradeRule
}

type Option func(*options)

func WithMasquerade(inputDev, outputDev string) Option {
	return func(o *options) {
		o.masqueradeRules = append(o.masqueradeRules, MasqueradeRule{
			InputDevice:  inputDev,
			OutputDevice: outputDev,
		})
	}
}
