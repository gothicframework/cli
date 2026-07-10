STAGE ?= dev


# To deploy your app and public folder
deploy:
	gothic deploy --action deploy --stage $(STAGE)

delete:
	gothic deploy --action delete --stage $(STAGE)

# Starts your Application in dev mode watching Templates, golang files and CSS files
dev:
	gothic hot-reload


# Compiles Templates to generate equivalent golang files
templ:
	gothic build


# Generate CSS based on classes located on templ files
css:
	gothic css

# Generate CSS based on classes located on templ files in watch mode
optimize-images:
	gothic optimize-images


